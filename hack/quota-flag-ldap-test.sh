#!/bin/sh
# Integration test of programs/quota_flag.py against a real OpenLDAP
# (chrroessner/openldap, the image of the mailstack) with the RNS mail
# schema, LDAPS, SASL EXTERNAL via a client certificate and the proposed
# provider ACL (the quota identity may write rnsMSOverQuota only).
# Everything is local and throwaway: a test PKI, a docker network, the
# directory container and the runtime base image with the programs' venv.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DATA="$ROOT/hack/testdata/quota-ldap"
LDAP_IMAGE=${LDAP_IMAGE:-chrroessner/openldap:2.6.15-r1@sha256:5ed9cdbb8bc68a08546e3bedffcaa02f584616591fba3c39c36c25fa474faa35}
RUNTIME_IMAGE=${RUNTIME_IMAGE:-quay.io/centos/centos:stream9}
NAME=shardpost-quota-ldap-test-$$
WORK=$(mktemp -d)

cleanup() {
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	docker network rm "$NAME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# Test PKI: CA, server leaf for "ldap", client leaves for the quota
# identity and for a stranger without rights.
cert() { # name cn eku
	openssl req -newkey rsa:2048 -nodes -keyout "$WORK/$1.key" -out "$WORK/$1.csr" -subj "/CN=$2" 2>/dev/null
	printf 'subjectAltName=DNS:%s\nextendedKeyUsage=%s\n' "$2" "$3" >"$WORK/$1.ext"
	openssl x509 -req -in "$WORK/$1.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial \
		-out "$WORK/$1.crt" -days 1 -extfile "$WORK/$1.ext" 2>/dev/null
}
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$WORK/ca.key" -out "$WORK/ca.crt" -days 1 -subj "/CN=quota test CA" 2>/dev/null
cert ldap ldap serverAuth
cert client shardpost-quota clientAuth
cert stranger stranger clientAuth
chmod 0644 "$WORK"/*

docker network create "$NAME" >/dev/null
docker run -d --name "$NAME" --network "$NAME" --network-alias ldap --platform linux/amd64 \
	-e LDAP_SKIP_DEFAULT_CONFIG=true -e LDAP_CONFIG_BACKEND=slapd.conf -e LDAP_CONF=/etc/openldap/slapd.conf \
	-e LDAP_ADMIN_DN=cn=admin,dc=roessner-net,dc=de -e LDAP_ADMIN_PASSWORD=synthetic-admin \
	-e LDAP_ENABLE_LDAPS=true -e LDAP_LISTEN_URIS="ldaps:/// ldapi:///" \
	-v "$DATA/slapd.conf:/etc/openldap/slapd.conf:ro" \
	-v "$DATA/rnsmailsystem.schema:/etc/openldap/custom-schema/rnsmailsystem.schema:ro" \
	-v "$DATA/10-accounts.ldif:/seed/10-accounts.ldif:ro" \
	-v "$WORK/ca.crt:/etc/openldap/tls/ca.crt:ro" -v "$WORK/ldap.crt:/etc/openldap/tls/ldap.crt:ro" \
	-v "$WORK/ldap.key:/etc/openldap/tls/ldap.key:ro" \
	"$LDAP_IMAGE" >/dev/null

# The entrypoint does not seed a custom slapd.conf; add the tree once
# slapd answers.
i=0
until docker exec "$NAME" sh -c 'LDAPTLS_CACERT=/etc/openldap/tls/ca.crt ldapadd -x -H ldaps://ldap -D cn=admin,dc=roessner-net,dc=de -w synthetic-admin -f /seed/10-accounts.ldif' >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then
		docker logs "$NAME" 2>&1 | grep -v " conn=" | tail -20
		echo "directory did not start" >&2
		exit 1
	fi
	sleep 1
done

# The programs' configuration mirrors the overlay (deploy/overlays/
# croessner-nauthilus/server.yaml); only host, CA and files differ.
docker run --rm --network "$NAME" --platform linux/amd64 -e PYTHONDONTWRITEBYTECODE=1 \
	-v "$ROOT/programs:/programs:ro" -v "$WORK:/pki:ro" "$RUNTIME_IMAGE" sh -eu -c '
dnf -q -y install python3 >/dev/null
python3 -m venv /tmp/venv
/tmp/venv/bin/pip -q --disable-pip-version-check install --require-hashes --no-deps --only-binary=:all: -r /programs/requirements.txt

export FLAG_LDAP_URI=ldaps://ldap:636 FLAG_LDAP_BASE=ou=people,ou=it,dc=roessner-net,dc=de \
	"FLAG_LDAP_FILTER=(&(objectClass=rnsMSDovecotAccount)(|(rnsMSDovecotUser={account})(rnsMSRecipientAddress={account})(uniqueIdentifier={account})))" \
	FLAG_LDAP_ATTRIBUTE=rnsMSOverQuota FLAG_LDAP_AUTH=external FLAG_LDAP_CA_FILE=/pki/ca.crt \
	FLAG_LDAP_CERT_FILE=/pki/client.crt FLAG_LDAP_KEY_FILE=/pki/client.key

fail=0
check() { # expected-exit expected-text env...
	want=$1 text=$2
	shift 2
	out=$(env "$@" /tmp/venv/bin/python /programs/quota_flag.py) && code=0 || code=$?
	if [ "$code" -eq "$want" ] && printf "%s" "$out" | grep -q "$text"; then
		echo "ok   $out"
	else
		echo "FAIL (exit $code, want $want \"$text\") $out"
		fail=1
	fi
}

check 0 "set TRUE" QUOTA_ACCOUNT=quotatest@example.org QUOTA_OVER=true
check 0 "already TRUE" QUOTA_ACCOUNT=quotatest@example.org QUOTA_OVER=true
check 0 "set FALSE" QUOTA_ACCOUNT=alias@example.org QUOTA_OVER=false
check 1 "0 LDAP entries" QUOTA_ACCOUNT=nobody@example.org QUOTA_OVER=true
check 1 "InsufficientAccessRights" QUOTA_ACCOUNT=quotatest@example.org QUOTA_OVER=true FLAG_LDAP_ATTRIBUTE=rnsMSQuota FLAG_LDAP_VALUE_OVER=1
check 1 "InsufficientAccessRights" QUOTA_ACCOUNT=quotatest@example.org QUOTA_OVER=true FLAG_LDAP_CERT_FILE=/pki/stranger.crt FLAG_LDAP_KEY_FILE=/pki/stranger.key
check 75 "LDAP bind" QUOTA_ACCOUNT=quotatest@example.org QUOTA_OVER=true FLAG_LDAP_CA_FILE=/pki/client.crt
exit $fail'

state=$(docker exec "$NAME" sh -c 'LDAPTLS_CACERT=/etc/openldap/tls/ca.crt ldapsearch -x -H ldaps://ldap -D cn=admin,dc=roessner-net,dc=de -w synthetic-admin -b ou=people,ou=it,dc=roessner-net,dc=de -LLL "(uid=quotatest)" rnsMSOverQuota rnsMSQuota' | grep '^rnsMS' | tr '\n' ' ')
echo "directory: $state"
case "$state" in
*"rnsMSOverQuota: FALSE"*"rnsMSQuota: 1073741824"* | *"rnsMSQuota: 1073741824"*"rnsMSOverQuota: FALSE"*) echo "quota-flag LDAP integration: ok" ;;
*)
	echo "unexpected directory state" >&2
	exit 1
	;;
esac
