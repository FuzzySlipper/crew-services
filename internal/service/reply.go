package service

import "crew-services/internal/store"

const (
	CodeReplyOriginalNotFound   ErrorCode = "reply_original_not_found"
	CodeReplySenderMismatch     ErrorCode = "reply_sender_mismatch"
	CodeReplyRecipientMismatch  ErrorCode = "reply_recipient_mismatch"
	CodeReplyGenerationMismatch ErrorCode = "reply_generation_mismatch"
)

func mapReplyError(err error) error {
	switch err {
	case store.ErrReplyOriginalNotFound:
		return &Error{Code: CodeReplyOriginalNotFound, Err: err}
	case store.ErrReplySenderMismatch:
		return &Error{Code: CodeReplySenderMismatch, Err: err}
	case store.ErrReplyRecipientMismatch:
		return &Error{Code: CodeReplyRecipientMismatch, Err: err}
	case store.ErrReplyGenerationMismatch:
		return &Error{Code: CodeReplyGenerationMismatch, Err: err}
	default:
		return mapStoreError(err)
	}
}
