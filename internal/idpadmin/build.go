package idpadmin

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/samllogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Builder turns stored configuration into the runtime login providers used by
// the browser login handlers. Origin is the externally visible HTTPS origin.
type Builder struct {
	Origin     string
	Transport  http.RoundTripper // OIDC outbound transport; nil means default
	DB         *pgxpool.Pool
	Keys       *secrets.Keyring
	InstanceID string
	Dial       directory.Dialer // nil means real LDAP network transport
}

func (b Builder) OIDC(ctx context.Context, l Loaded) (*oidclogin.Provider, error) {
	var c OIDCConfig
	if err := strict(l.Config, &c); err != nil {
		return nil, err
	}
	return oidclogin.NewProviderWithTransport(ctx, oidclogin.ProviderConfig{
		Key: "idp-" + l.ID, Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: string(l.Secret),
		RedirectURI: b.Origin + "/auth/oidc/callback", AllowedEndpoints: c.AllowedEndpoints,
		RolesClaim: c.RolesClaim, ClaimRoles: claimRoleMap(c.ClaimRoles), Scopes: c.Scopes, NoEndSession: c.NoEndSession,
	}, b.Transport)
}

// SAML builds the service provider. replay must be durable in production.
func (b Builder) SAML(l Loaded, replay samllogin.ReplayStore) (*samllogin.Provider, error) {
	var c SAMLConfig
	if err := strict(l.Config, &c); err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair([]byte(c.SPCertificatePEM), l.Secret)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, ErrInvalid
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, ErrInvalid
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, ErrInvalid
	}
	return samllogin.New(samllogin.Config{
		EntityID: c.EntityID, IDPEntityID: c.IDPEntityID, ACSURL: b.Origin + "/auth/saml/callback", MetadataURL: b.Origin + "/auth/saml/metadata", LogoutURL: b.Origin + "/auth/saml/slo",
		Metadata: []byte(c.IDPMetadataXML), Key: signer, Certificate: cert, Replay: replay,
		GroupsAttribute: c.GroupsAttribute, EmailAttribute: c.EmailAttribute,
		RolesAttribute: c.RolesAttribute, ClaimRoles: claimRoleMap(c.ClaimRoles),
	})
}

// LDAP builds the directory provider. When the provider is stored, the bind
// password is re-read from encrypted storage on every operation rather than
// being held by the provider.
func (b Builder) LDAP(l Loaded) (*directory.Provider, error) {
	var c LDAPConfig
	if err := strict(l.Config, &c); err != nil {
		return nil, err
	}
	static := append([]byte(nil), l.Secret...)
	id := l.ID
	secret := func(ctx context.Context) ([]byte, time.Time, error) {
		if b.DB != nil && b.Keys != nil && id != "" && len(static) == 0 {
			var sid string
			if err := b.DB.QueryRow(ctx, `SELECT secret_id::text FROM identity_providers WHERE instance_id=$1 AND id::text=$2 AND secret_id IS NOT NULL`, b.InstanceID, id).Scan(&sid); err != nil {
				return nil, time.Time{}, directory.ErrUnavailable
			}
			_, plain, err := (secrets.Store{DB: b.DB, Keys: b.Keys}).Open(ctx, b.InstanceID, sid, purposePrefix+id)
			if err != nil {
				return nil, time.Time{}, directory.ErrUnavailable
			}
			return plain, time.Now().Add(time.Minute), nil
		}
		return append([]byte(nil), static...), time.Now().Add(time.Minute), nil
	}
	return directory.New(directory.Config{
		ProviderID: c.ProviderID, URL: c.URL, BindDN: c.BindDN, UserBaseDN: c.UserBaseDN, GroupBaseDN: c.GroupBaseDN,
		LoginAttribute: c.LoginAttribute, UserIDAttribute: c.UserIDAttribute, GroupIDAttribute: c.GroupIDAttribute,
		UserClass: c.UserClass, GroupClass: c.GroupClass, MemberAttribute: c.MemberAttribute,
		CAPEM: []byte(c.CAPEM), GroupRoles: c.GroupRoles, GroupNesting: c.GroupNesting,
	}, secret, b.Dial)
}

func claimRoleMap(in []ClaimRole) map[string][]string {
	out := make(map[string][]string, len(in))
	for _, m := range in {
		out[m.Value] = m.RoleIDs
	}
	return out
}

type denyReplay struct{}

func (denyReplay) Claim(context.Context, []string, time.Time) (bool, error) { return false, nil }

// Check validates a candidate configuration by building it: OIDC performs
// discovery against the approved endpoints; SAML and LDAP construct the provider
// (parsing metadata/keys/TLS roots). It never contacts a directory.
func (b Builder) Check(ctx context.Context, l Loaded) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := Validate(l.Kind, l.Config); err != nil {
		return err
	}
	switch l.Kind {
	case OIDC:
		_, err := b.OIDC(ctx, l)
		return err
	case SAML:
		_, err := b.SAML(l, denyReplay{})
		return err
	default:
		_, err := b.LDAP(l)
		return err
	}
}

// SPMetadata renders the service provider metadata for a SAML provider.
func (b Builder) SPMetadata(l Loaded) ([]byte, error) {
	p, err := b.SAML(l, denyReplay{})
	if err != nil {
		return nil, err
	}
	return p.Metadata()
}

// pemCertificates decodes every CERTIFICATE block.
func pemCertificates(data []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, c)
		}
	}
}

// rawConfig is a helper for callers needing the typed SAML config.
func SAMLConfigOf(raw json.RawMessage) (SAMLConfig, error) {
	var c SAMLConfig
	err := strict(raw, &c)
	return c, err
}
