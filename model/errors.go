package model

import "errors"

// Common errors
var (
	ErrDatabase = errors.New("database error")
)

// User auth errors
var (
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrUserEmptyCredentials = errors.New("empty credentials")
	ErrEmailAlreadyTaken    = errors.New("email already taken")
	ErrEmailNotFound        = errors.New("email not found")
	ErrEmailAmbiguous       = errors.New("email matches multiple users")
	// ErrLoginIdentifierTaken means the string is already another account's
	// username or email, so accepting it would make that identifier name two
	// accounts and break login for one of them.
	ErrLoginIdentifierTaken = errors.New("login identifier already taken")
	// ErrLoginIdentifierAmbiguous means the identifier names more than one
	// account, so no account can be proven. An administrator has to resolve the
	// collision; login must not guess one side of it.
	ErrLoginIdentifierAmbiguous = errors.New("login identifier matches multiple accounts")
)

// Token auth errors
var (
	ErrTokenNotProvided = errors.New("token not provided")
	ErrTokenInvalid     = errors.New("token invalid")
)

// Redemption errors
var ErrRedeemFailed = errors.New("redeem.failed")

// 2FA errors
var ErrTwoFANotEnabled = errors.New("2fa not enabled")
var ErrTwoFAAlreadyEnabled = errors.New("2fa already enabled")
