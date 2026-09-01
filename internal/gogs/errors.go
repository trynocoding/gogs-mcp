package gogs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"

	"github.com/cockroachdb/errors"
)

type ErrorCode string

const (
	CodeAuthenticationFailed        ErrorCode = "AUTHENTICATION_FAILED"
	CodePermissionDenied            ErrorCode = "PERMISSION_DENIED"
	CodeResourceNotFoundOrForbidden ErrorCode = "RESOURCE_NOT_FOUND_OR_FORBIDDEN"
	CodeConflict                    ErrorCode = "CONFLICT"
	CodeValidationFailed            ErrorCode = "VALIDATION_FAILED"
	CodeRateLimited                 ErrorCode = "RATE_LIMITED"
	CodeUnavailable                 ErrorCode = "GOGS_UNAVAILABLE"
	CodeGogsError                   ErrorCode = "GOGS_ERROR"
	CodeTLSError                    ErrorCode = "TLS_ERROR"
	CodeTimeout                     ErrorCode = "TIMEOUT"
	CodeResponseTooLarge            ErrorCode = "RESPONSE_TOO_LARGE"
	CodeInternal                    ErrorCode = "INTERNAL_ERROR"
)

type Error struct {
	Code       ErrorCode
	Message    string
	Retryable  bool
	HTTPStatus int
	cause      error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.cause
}

func AsError(err error) *Error {
	var gogsError *Error
	if errors.As(err, &gogsError) {
		return gogsError
	}
	return &Error{
		Code:    CodeInternal,
		Message: "An internal error occurred.",
	}
}

func classifyTransportError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Code:      CodeTimeout,
			Message:   "The Gogs request timed out.",
			Retryable: true,
			cause:     err,
		}
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &Error{
			Code:      CodeTimeout,
			Message:   "The Gogs request timed out.",
			Retryable: true,
			cause:     err,
		}
	}

	var unknownAuthority x509.UnknownAuthorityError
	var certificateInvalid x509.CertificateInvalidError
	var hostnameError x509.HostnameError
	var verificationError *tls.CertificateVerificationError
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) ||
		errors.As(err, &certificateInvalid) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &verificationError) ||
		errors.As(err, &recordHeaderError) {
		return &Error{
			Code:    CodeTLSError,
			Message: "TLS verification for Gogs failed.",
			cause:   err,
		}
	}

	return &Error{
		Code:      CodeUnavailable,
		Message:   "Gogs is unavailable.",
		Retryable: true,
		cause:     err,
	}
}

func classifyStatus(status int) *Error {
	switch status {
	case 401:
		return &Error{Code: CodeAuthenticationFailed, Message: "Gogs rejected the configured credentials.", HTTPStatus: status}
	case 403:
		return &Error{Code: CodePermissionDenied, Message: "Gogs denied this operation.", HTTPStatus: status}
	case 404:
		return &Error{Code: CodeResourceNotFoundOrForbidden, Message: "The resource does not exist or the current user cannot access it.", HTTPStatus: status}
	case 409:
		return &Error{Code: CodeConflict, Message: "The request conflicts with the current Gogs state.", HTTPStatus: status}
	case 422:
		return &Error{Code: CodeValidationFailed, Message: "Gogs rejected the request data.", HTTPStatus: status}
	case 429:
		return &Error{Code: CodeRateLimited, Message: "Gogs or its proxy rate limited the request.", Retryable: true, HTTPStatus: status}
	case 502, 503, 504:
		return &Error{Code: CodeUnavailable, Message: "Gogs is unavailable.", Retryable: true, HTTPStatus: status}
	default:
		return &Error{Code: CodeGogsError, Message: "Gogs returned an unexpected response.", Retryable: status >= 500, HTTPStatus: status}
	}
}
