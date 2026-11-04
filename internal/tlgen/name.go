package tlgen

import "strings"

// goKeywords require renaming when used as identifiers.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true,
	"for": true, "func": true, "go": true, "goto": true, "if": true,
	"import": true, "interface": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true,
	"switch": true, "type": true, "var": true,
}

// initialisms upgrade common TL contractions to Go style. Keys are compared
// case-insensitively against whole pascal parts.
var initialisms = map[string]string{
	"id": "ID", "ip": "IP", "url": "URL", "uri": "URI", "api": "API",
	"http": "HTTP", "https": "HTTPS", "json": "JSON", "md5": "MD5",
	"ttl": "TTL", "dc": "DC", "ok": "OK", "pts": "PTS", "qts": "QTS",
	"p2p": "P2P", "sha1": "SHA1", "sha256": "SHA256", "qr": "QR",
	"cdn": "CDN", "rtc": "RTC", "rtmp": "RTMP", "vpn": "VPN", "psa": "PSA",
	"ipv4": "IPv4", "ipv6": "IPv6", "iv": "IV", "dh": "DH", "rsa": "RSA",
	"srp": "SRP", "tmp": "TMP", "gb": "GB", "ga": "GA",
}

// pascalPart converts one snake/camel part to PascalCase with initialisms.
func pascalPart(s string) string {
	if s == "" {
		return s
	}
	up := strings.ToUpper(s[:1]) + s[1:]
	if v, ok := initialisms[strings.ToLower(s)]; ok {
		return v
	}
	return up
}

// pascal converts a TL identifier to PascalCase. Snake parts are split;
// camelCase names keep their interior capitalization.
func pascal(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(pascalPart(p))
	}
	return b.String()
}

// goTypeDefName is the Go struct name for a definition.
func goTypeDefName(d *Def) string {
	if d.Namespace == "" {
		return pascal(d.Name)
	}
	return pascal(d.Namespace) + pascal(d.Name)
}

// goFuncName is the Go struct name for a function definition.
func goFuncName(d *Def) string {
	return goTypeDefName(d) + "Request"
}

// goInterfaceName is the Go interface name for an abstract result type.
func goInterfaceName(ns, name string) string {
	if ns == "" {
		return pascal(name) + "Class"
	}
	return pascal(ns) + pascal(name) + "Class"
}

// goFieldName converts a TL argument name to an exported Go field name.
func goFieldName(name string) string {
	g := pascal(name)
	if goKeywords[strings.ToLower(g)] || goKeywords[g] {
		return g + "_"
	}
	return g
}
