// Package renew keeps an LDAP client certificate of the UCM private
// PKI current inside Kubernetes: it checks the certificate in a Secret,
// and when it is missing, foreign or close to expiry, it creates a new
// key and CSR, has the CSR signed by the constrained UCM signer over SSH,
// verifies the result against pinned trust anchors and writes it back.
// The private key never leaves the job; the signer only sees the CSR.
package renew

import (
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"strings"
)

// Attribute types of the subjects the UCM signer issues, with the ASN.1
// string types the signer (Python cryptography) uses for them.
var attributeTypes = map[string]struct {
	oid asn1.ObjectIdentifier
	tag int
}{
	"C":            {asn1.ObjectIdentifier{2, 5, 4, 6}, asn1.TagPrintableString},
	"DC":           {asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 25}, asn1.TagIA5String},
	"ST":           {asn1.ObjectIdentifier{2, 5, 4, 8}, asn1.TagUTF8String},
	"L":            {asn1.ObjectIdentifier{2, 5, 4, 7}, asn1.TagUTF8String},
	"O":            {asn1.ObjectIdentifier{2, 5, 4, 10}, asn1.TagUTF8String},
	"OU":           {asn1.ObjectIdentifier{2, 5, 4, 11}, asn1.TagUTF8String},
	"CN":           {asn1.ObjectIdentifier{2, 5, 4, 3}, asn1.TagUTF8String},
	"emailAddress": {asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}, asn1.TagIA5String},
}

// Attribute is one subject attribute in the order the signer expects.
type Attribute struct {
	Type  asn1.ObjectIdentifier
	Tag   int
	Value string
}

// Subject is an ordered certificate subject.
type Subject []Attribute

// ParseSubject parses "C=DE,DC=de,...,CN=name,emailAddress=x" in the
// order given (first element outermost, like the signer's attribute
// list). Values must not contain commas or equal signs.
func ParseSubject(spec string) (Subject, error) {
	var s Subject

	for _, part := range strings.Split(spec, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		t, known := attributeTypes[key]

		if !ok || !known || value == "" || strings.ContainsAny(value, "=+\\\"<>;") || !fitsType(t.tag, value) {
			return nil, fmt.Errorf("subject attribute %q is not supported", part)
		}

		s = append(s, Attribute{Type: t.oid, Tag: t.tag, Value: value})
	}

	if s.CommonName() == "" {
		return nil, fmt.Errorf("subject needs a CN")
	}

	return s, nil
}

// fitsType reports whether value is valid for the ASN.1 string type:
// PrintableString and IA5String take ASCII only (printable subset for
// the former), UTF8String anything.
func fitsType(tag int, value string) bool {
	if tag == asn1.TagUTF8String {
		return true
	}

	for _, r := range value {
		switch {
		case r > 0x7e || r < 0x20:
			return false
		case tag == asn1.TagPrintableString && !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 '()+,-./:=?", r):
			return false
		}
	}

	return true
}

// CommonName returns the CN.
func (s Subject) CommonName() string {
	for _, a := range s {
		if a.Type.Equal(attributeTypes["CN"].oid) {
			return a.Value
		}
	}

	return ""
}

// atv is one attribute with an explicit string type.
type atv struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue
}

// rdnSET is one relative distinguished name; the SET suffix makes
// encoding/asn1 encode it as SET OF (like pkix.RelativeDistinguishedNameSET).
type rdnSET []atv

// DER encodes the subject as an RDN sequence, one attribute per RDN,
// with the signer's string types.
func (s Subject) DER() ([]byte, error) {
	rdns := make([]rdnSET, 0, len(s))
	for _, a := range s {
		rdns = append(rdns, rdnSET{{Type: a.Type, Value: asn1.RawValue{Tag: a.Tag, Bytes: []byte(a.Value)}}})
	}

	return asn1.Marshal(rdns)
}

// Matches reports whether name has exactly these attributes in this
// order (string types aside).
func (s Subject) Matches(name pkix.Name) bool {
	if len(name.Names) != len(s) {
		return false
	}

	for i, a := range s {
		got := name.Names[i]
		if !got.Type.Equal(a.Type) || fmt.Sprint(got.Value) != a.Value {
			return false
		}
	}

	return true
}
