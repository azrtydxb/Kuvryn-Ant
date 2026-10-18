// Package idpadmin manages database-stored sign-in providers (OIDC, SAML,
// LDAP): validated configuration, encrypted secrets, runtime construction,
// connectivity diagnostics and claim/group-mapping previews. It never creates
// users, bindings or roles; login still requires an existing identity binding.
package idpadmin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

type Kind string

const (
	OIDC Kind = "oidc"
	SAML Kind = "saml"
	LDAP Kind = "ldap"
)

func (k Kind) Valid() bool { return k == OIDC || k == SAML || k == LDAP }

var (
	ErrInvalid  = errors.New("invalid provider configuration")
	ErrNotFound = errors.New("provider not found")
	ErrConflict = errors.New("provider name already exists")
	ErrRevision = errors.New("provider revision changed")
)

// Provider is the administrator view; it never includes secret material.
type Provider struct {
	ID        string          `json:"id"`
	Kind      Kind            `json:"kind"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Config    json.RawMessage `json:"config"`
	HasSecret bool            `json:"has_secret"`
	Revision  int64           `json:"revision"`
	LastError string          `json:"last_error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Loaded pairs a provider with its decrypted secret. Callers must clear Secret.
type Loaded struct {
	Provider
	Secret []byte
}

type OIDCConfig struct {
	Issuer           string   `json:"issuer"`
	ClientID         string   `json:"client_id"`
	AllowedEndpoints []string `json:"allowed_endpoints"`
	GroupsClaim      string   `json:"groups_claim,omitempty"`
}

type SAMLConfig struct {
	EntityID         string `json:"entity_id"`
	IDPEntityID      string `json:"idp_entity_id"`
	IDPMetadataXML   string `json:"idp_metadata_xml"`
	SPCertificatePEM string `json:"sp_certificate_pem,omitempty"`
	GroupsAttribute  string `json:"groups_attribute,omitempty"`
	EmailAttribute   string `json:"email_attribute,omitempty"`
}

type LDAPConfig struct {
	ProviderID       string              `json:"provider_id"`
	URL              string              `json:"url"`
	BindDN           string              `json:"bind_dn"`
	CAPEM            string              `json:"ca_pem,omitempty"`
	UserBaseDN       string              `json:"user_base_dn"`
	GroupBaseDN      string              `json:"group_base_dn"`
	LoginAttribute   string              `json:"login_attribute"`
	UserIDAttribute  string              `json:"user_id_attribute"`
	GroupIDAttribute string              `json:"group_id_attribute"`
	UserClass        string              `json:"user_class"`
	GroupClass       string              `json:"group_class"`
	MemberAttribute  string              `json:"member_attribute"`
	GroupRoles       map[string][]string `json:"group_roles,omitempty"`
}

// strict decodes exactly one JSON object with no unknown or duplicate-case keys.
func strict(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.More() {
		return ErrInvalid
	}
	return nil
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

// Validate performs structural validation. Protocol-level validation (does the
// discovery document match, does the bind work) is the Builder's job.
func Validate(kind Kind, config json.RawMessage) error {
	if len(config) == 0 || len(config) > 512<<10 {
		return ErrInvalid
	}
	switch kind {
	case OIDC:
		var c OIDCConfig
		if strict(config, &c) != nil || !httpsURL(c.Issuer) || strings.Contains(c.Issuer, "?") || c.ClientID == "" || len(c.ClientID) > 256 || len(c.GroupsClaim) > 64 {
			return ErrInvalid
		}
		if len(c.AllowedEndpoints) == 0 || len(c.AllowedEndpoints) > 32 {
			return ErrInvalid
		}
		for _, e := range c.AllowedEndpoints {
			if !httpsURL(e) {
				return ErrInvalid
			}
		}
	case SAML:
		var c SAMLConfig
		if strict(config, &c) != nil || c.EntityID == "" || len(c.EntityID) > 2048 || c.IDPEntityID == "" || len(c.IDPEntityID) > 2048 || c.IDPMetadataXML == "" || len(c.IDPMetadataXML) > 256<<10 || len(c.GroupsAttribute) > 256 || len(c.EmailAttribute) > 256 {
			return ErrInvalid
		}
	case LDAP:
		var c LDAPConfig
		if strict(config, &c) != nil || !httpsURL(c.ProviderID) || len(c.ProviderID) > 256 || c.URL == "" || c.BindDN == "" || len(c.CAPEM) > 64<<10 || len(c.GroupRoles) > 1000 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Input creates or updates a provider. Secret nil leaves the stored secret
// unchanged on update; on create it is required for OIDC-confidential/LDAP.
type Input struct {
	Kind    Kind
	Name    string
	Enabled bool
	Config  json.RawMessage
	Secret  *string
	// RotateSPCertificate regenerates the SAML SP key pair (SAML only).
	RotateSPCertificate bool
}

func validName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(n) == n
}
