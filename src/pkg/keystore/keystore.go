// Package keystore is the hive-side face of crux/keystore: the same provider
// interface, envelope and secret-file helpers, re-exported so hive packages
// import one path, plus the hook network-backed providers (OpenBao Transit,
// hivecommons/hive#18216) register through. See crux/keystore for the design
// (hivecommons/hive#18214).
package keystore

import (
	"github.com/hivecommons/hive/crux/keystore"
)

// Provider, Envelope and Status are the crux types.
type (
	Provider       = keystore.Provider
	Envelope       = keystore.Envelope
	Status         = keystore.Status
	Factory        = keystore.Factory
	EnvKeyProvider = keystore.EnvKeyProvider
)

// Environment variable names and provider names.
const (
	EnvProviderName         = keystore.EnvProviderName
	EnvSecretsKey           = keystore.EnvSecretsKey
	EnvSecretsKeyFile       = keystore.EnvSecretsKeyFile
	EnvSecretsKeyPrevious   = keystore.EnvSecretsKeyPrevious
	EnvHeartbeatKey         = keystore.EnvHeartbeatKey
	EnvHeartbeatKeyPrevious = keystore.EnvHeartbeatKeyPrevious
	ProviderEnv             = keystore.ProviderEnv
	ProviderNone            = keystore.ProviderNone
	Prefix                  = keystore.Prefix
)

// Sentinel errors.
var (
	ErrNotEnvelope          = keystore.ErrNotEnvelope
	ErrMalformedEnvelope    = keystore.ErrMalformedEnvelope
	ErrNoProvider           = keystore.ErrNoProvider
	ErrProviderMismatch     = keystore.ErrProviderMismatch
	ErrUnknownKeyGeneration = keystore.ErrUnknownKeyGeneration
	ErrAuthentication       = keystore.ErrAuthentication
)

// Functions.
var (
	Init                   = keystore.Init
	Active                 = keystore.Active
	SetActive              = keystore.SetActive
	ResetForTest           = keystore.ResetForTest
	InitError              = keystore.InitError
	LogStatus              = keystore.LogStatus
	Register               = keystore.Register
	Registered             = keystore.Registered
	NewEnvKeyProvider      = keystore.NewEnvKeyProvider
	Seal                   = keystore.Seal
	Open                   = keystore.Open
	SealActive             = keystore.SealActive
	OpenActive             = keystore.OpenActive
	IsEnvelope             = keystore.IsEnvelope
	IsEnvelopeString       = keystore.IsEnvelopeString
	ParseEnvelope          = keystore.ParseEnvelope
	WriteFile              = keystore.WriteFile
	WriteFileMode          = keystore.WriteFileMode
	ReadFile               = keystore.ReadFile
	ReadString             = keystore.ReadString
	MigrateDir             = keystore.MigrateDir
	FileAAD                = keystore.FileAAD
	Rewritable             = keystore.Rewritable
	SetRewriteRootsForTest = keystore.SetRewriteRootsForTest
)
