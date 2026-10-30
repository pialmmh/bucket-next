// Package names holds the spelling rules of the two names every call carries: the tenant
// and the entity.
//
// The tenant is tenant-master's tenant id (the word prime-context's roots carry: btcl = the
// root schema = the topic suffix = the tenant id). It is never case-folded: BTCL is refused,
// not mapped to btcl.
package names

import "regexp"

// TenantRule is tenant-master's tenant id shape: lowercase letters and digits, a letter
// first, 2 to 16 characters.
const TenantRule = `^[a-z][a-z0-9]{1,15}$`

// EntityRule is the entity name shape: letters, digits, '.', '_' and '-', 1 to 64 characters.
const EntityRule = `^[A-Za-z0-9._-]{1,64}$`

var (
	tenantRe = regexp.MustCompile(TenantRule)
	entityRe = regexp.MustCompile(EntityRule)
)

// ValidTenant reports whether s is a well-formed tenant id.
func ValidTenant(s string) bool { return tenantRe.MatchString(s) }

// ValidEntity reports whether s is a well-formed entity name.
func ValidEntity(s string) bool { return entityRe.MatchString(s) }
