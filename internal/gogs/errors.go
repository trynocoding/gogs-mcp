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
	CodeInvalidArgument             ErrorCode = "INVALID_ARGUMENT"
	CodeArchiveUnsafe               ErrorCode = "ARCHIVE_UNSAFE"
	CodeSearchTimeout               ErrorCode = "SEARCH_TIMEOUT"
	CodeCacheCapacityExceeded       ErrorCode = "CACHE_CAPACITY_EXCEEDED"
	CodeWriteOutcomeUnknown         ErrorCode = "WRITE_OUTCOME_UNKNOWN"
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

// classifyWriteTransportError classifies a failed POST request. A dial or
// TLS handshake failure means the request never left the client, so the
// regular transport classification applies and the write may be retried.
// Every other transport failure leaves the outcome on the server unknown,
// which callers must treat as WRITE_OUTCOME_UNKNOWN.
func classifyWriteTransportError(err error) *Error {
	var opError *net.OpError
	if errors.As(err, &opError) && (opError.Op == "dial" || opError.Op == "proxyconnect") {
		return classifyTransportError(err)
	}
	if classified := classifyTransportError(err); classified.Code == CodeTLSError {
		return classified
	}
	return &Error{
		Code:    CodeWriteOutcomeUnknown,
		Message: "The write was sent to Gogs but its outcome is unknown; check the result before retrying.",
		cause:   err,
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
