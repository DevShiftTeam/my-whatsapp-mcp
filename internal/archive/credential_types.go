package archive

import (
	"context"
	"time"
)

type CredentialID string

type CredentialPolicy struct {
	Account     string
	AllChats    bool     // explicit owner-selected persisted grant, never request/default authority
	Chats       []ChatID // empty denies unless AllChats is explicitly true
	From, Until time.Time
	Rolling     time.Duration
}
type CredentialCreate struct {
	Label, Principal string
	Digest           [32]byte
	Policy           CredentialPolicy
	ExpiresAt        time.Time
}
type Credential struct {
	ID                   CredentialID
	Label, Principal     string
	Revision             uint64
	Policy               CredentialPolicy
	CreatedAt, ExpiresAt time.Time
	Revoked              bool
}
type CredentialGrant struct {
	CredentialID       CredentialID
	CredentialRevision uint64
	Scope              ReadScope
}
type CredentialStore interface {
	CreateCredential(context.Context, CredentialCreate) (Credential, error)
	ResolveCredential(context.Context, [32]byte) (CredentialGrant, error)
	ResolveCredentialID(context.Context, CredentialID) (CredentialGrant, error)
	SetCredentialPolicy(context.Context, CredentialID, CredentialPolicy, uint64) (Credential, error)
	RevokeCredential(context.Context, CredentialID, uint64) error
	CheckCredentialGrant(context.Context, CredentialGrant) error
}

const NativeClientPrincipalPrefix = "wa-indexer/native-client/"
const LegacyNativeClientPrincipal = "native-local-client"
const RemoteClientPrincipalPrefix = "wa-indexer/remote-client/"

type CredentialNamespace uint8

const CredentialNamespaceNativeClients CredentialNamespace = 1
const CredentialNamespaceRemoteClients CredentialNamespace = 2

// CredentialMetadata is owner recovery information and carries no authority.
type CredentialMetadata struct {
	ID                   CredentialID
	Account, Principal   string
	Revision             uint64
	CreatedAt, ExpiresAt time.Time
	Revoked              bool
}
type CredentialListRequest struct {
	Account, Principal string
	Namespace          CredentialNamespace
	After              CredentialID
	Limit              int
}
type CredentialMetadataPage struct {
	Items     []CredentialMetadata
	NextAfter CredentialID
}
type CredentialRecoveryStore interface {
	ListCredentials(context.Context, CredentialListRequest) (CredentialMetadataPage, error)
}
