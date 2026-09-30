require ["fileinto", "regex"];

# Runs after the account's own script for every delivery (sieve.after_dir)
# and sorts what that script left in INBOX: operational notifications and
# mailing lists go into public folders. It acts for the accounts that hold
# the post right (p) on the folder; for everybody else the fileinto is
# refused and the message stays in INBOX. Taken over from the sieve_after
# script of the mailstack (public-mls.sieve), with the folders below
# "Public/".

if address :contains "to" "postfix-notify@srvint.net" {
  fileinto "Public/Postfix";
  stop;
}

if address :contains "from" "zabbix@roessner-net.de" {
  fileinto "Public/Zabbix";
  stop;
}

if header :contains "List-Id" "dane-users.sys4.de" {
  fileinto "Public/Mailinglists/DANE-Users";
  stop;
}

if header :contains "List-Id" "dane.ietf.org" {
  fileinto "Public/Mailinglists/IETF-Dane";
  stop;
}

if header :contains "List-Id" "dovecot.dovecot.org" {
  fileinto "Public/Mailinglists/Dovecot";
  stop;
}

if anyof (
  header :contains "List-Post" "mailto:postfix-users@postfix.org",
  header :contains "List-Post" "mailto:postfix-announce@postfix.org"
) {
  fileinto "Public/Mailinglists/Postfix";
  stop;
}

if header :contains "List-Id" "<pdns-users.mailman.powerdns.com>" {
  fileinto "Public/Mailinglists/PowerDNS";
  stop;
}

if header :contains "List-Id" "amavis-users.amavis.org" {
  fileinto "Public/Mailinglists/Amavis";
  stop;
}

if anyof (
  header :contains "List-Id" "automx-users.automx.org",
  header :contains "List-Id" "automx-users.sys4.de"
) {
  fileinto "Public/Mailinglists/Automx";
  stop;
}

if header :contains "List-Id" "opendkim-users.lists.opendkim.org" {
  fileinto "Public/Mailinglists/OpenDKIM";
  stop;
}

if header :contains "List-Id" "opendmarc-users.trusteddomain.org" {
  fileinto "Public/Mailinglists/OpenDMARC";
  stop;
}

if allof (
  header :contains "List-Id" "users.sogo.nu",
  not header :regex "subject" "^\\[SOGo\\] ANN: SOGo.*$"
) {
  fileinto "Public/Mailinglists/SOGo";
  stop;
}

if header :contains "List-Id" "zabbix-users.lists.sourceforge.net" {
  fileinto "Public/Mailinglists/Zabbix";
  stop;
}

if header :contains "List-Id" "<lartc.vger.kernel.org>" {
  fileinto "Public/Mailinglists/LARTC";
  stop;
}

if header :contains "List-Post" "gentoo-user@lists.gentoo.org" {
  fileinto "Public/Mailinglists/Gentoo-user";
  stop;
}

if header :contains "List-Post" "gentoo-announce@lists.gentoo.org" {
  fileinto "Public/Mailinglists/Gentoo-announce";
  stop;
}

if address :contains "from" "bugzilla-daemon@gentoo.org" {
  fileinto "Public/Mailinglists/Gentoo-bugzilla";
  stop;
}

if header :contains "List-ID" "announce.openbsd.org" {
  fileinto "Public/Mailinglists/OpenBSD-announce";
  stop;
}

if header :contains "List-ID" "misc.openbsd.org" {
  fileinto "Public/Mailinglists/OpenBSD-misc";
  stop;
}

if anyof (
  header :contains "List-Id" "rspamd.vstakhov.github.com",
  header :contains "List-ID" "rspamd/rspamd <rspamd.rspamd.github.com>",
  header :contains "List-Id" "User questions <users.lists.rspamd.com>"
) {
  fileinto "Public/Mailinglists/Rspamd";
  stop;
}

if header :contains "List-Id" "pymilter.gathman.org" {
  fileinto "Public/Mailinglists/PyMilter";
  stop;
}

if header :contains "List-Id" "samba.lists.samba.org" {
  fileinto "Public/Mailinglists/Samba";
  stop;
}

if header :contains "List-Id" "samba-announce.lists.samba.org" {
  fileinto "Public/Mailinglists/Samba-announce";
  stop;
}
