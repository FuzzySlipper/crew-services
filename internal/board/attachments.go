package board

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// AttachmentStore keeps attachment records; the bytes live with the Ingester.
type AttachmentStore interface {
	PutAttachment(ctx context.Context, a Attachment) error
	GetAttachment(ctx context.Context, ref string) (Attachment, error)
	AttachmentByOriginal(ctx context.Context, original string) (Attachment, error)
	ListAttachments(ctx context.Context, state string) ([]Attachment, error)
}

// UseMedia turns on attachments. Without it uploads are refused.
func (s *Service) UseMedia(in *Ingester, store AttachmentStore) {
	s.media, s.attachments = in, store
}

// blobRef is how a post names an attachment: blob:sha256-<ref>.
var blobRef = regexp.MustCompile(`blob:sha256-([0-9a-f]{64})`)

func attachmentRefs(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range blobRef.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// checkAttachments refuses a post that names an attachment this site does
// not have, or whose attachments add up to more than the per-post cap.
func (s *Service) checkAttachments(ctx context.Context, body string) error {
	refs := attachmentRefs(body)
	if len(refs) == 0 {
		return nil
	}
	if s.media == nil {
		return refuse(CodeAttachmentUnknown, "attachments are not set up on this site")
	}
	var total int64
	for _, ref := range refs {
		a, err := s.attachments.GetAttachment(ctx, ref)
		if errors.Is(err, ErrNotFound) || err == nil && a.State == AttachmentDropped {
			return refuse(CodeAttachmentUnknown, "no attachment %s on this site; upload it first", short(ref))
		}
		if err != nil {
			return err
		}
		total += a.Bytes
	}
	if limit := s.media.cfg.PostBytes; limit > 0 && total > limit {
		return refuse(CodeMediaTooLarge, "the post's attachments add up to %d MB; a post carries at most %d MB", total>>20, limit>>20)
	}
	return nil
}

func short(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// withAttachments fills in what a post's attachments are, for display.
func (s *Service) withAttachments(ctx context.Context, post Post) Post {
	if s.attachments == nil || post.State != PostVisible {
		return post
	}
	for _, ref := range attachmentRefs(post.Body) {
		if a, err := s.attachments.GetAttachment(ctx, ref); err == nil {
			post.Attachments = append(post.Attachments, a)
		}
	}
	return post
}

// Upload runs an upload through ingest. The same bytes uploaded again come
// back as the attachment already made from them.
func (s *Service) Upload(ctx context.Context, name string, data []byte) (Attachment, error) {
	if s.media == nil {
		return Attachment{}, refuse(CodeMediaTypeRefused, "attachments are not set up on this site")
	}
	if a, err := s.attachments.AttachmentByOriginal(ctx, digest(data)); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Attachment{}, err
	}
	a, err := s.media.Ingest(ctx, name, data, Skip{})
	if err != nil {
		return Attachment{}, err
	}
	if a.State == AttachmentHeld {
		a.Bytes = int64(len(data))
	}
	return a, s.attachments.PutAttachment(ctx, a)
}

func (s *Service) Attachment(ctx context.Context, ref string) (Attachment, error) {
	if s.attachments == nil {
		return Attachment{}, refuse(CodeAttachmentUnknown, "attachments are not set up on this site")
	}
	a, err := s.attachments.GetAttachment(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return Attachment{}, refuse(CodeAttachmentUnknown, "no attachment %s", short(ref))
	}
	return a, err
}

// BlobPath is where a ready attachment's bytes are; a held or removed one
// has none to give.
func (s *Service) BlobPath(ctx context.Context, ref string) (Attachment, string, error) {
	a, err := s.Attachment(ctx, ref)
	if err != nil {
		return Attachment{}, "", err
	}
	if a.State != AttachmentReady {
		return a, "", &Error{Code: CodeAttachmentHeld, Err: errors.New("attachment " + short(ref) + " is " + a.State + ": " + a.Reason)}
	}
	return a, s.media.BlobPath(a), nil
}

// Held lists attachments waiting for a person.
func (s *Service) Held(ctx context.Context) ([]Attachment, error) {
	if s.attachments == nil {
		return []Attachment{}, nil
	}
	return s.attachments.ListAttachments(ctx, AttachmentHeld)
}

// HeldRequest releases or drops a held attachment. Only a person on this site
// may.
type HeldRequest struct {
	Actor
	Ref string `json:"ref"`
}

func (s *Service) held(ctx context.Context, request HeldRequest) (Attachment, error) {
	if strings.TrimSpace(request.Person) == "" {
		return Attachment{}, refuse(CodePersonOnly, "only a person on this site can release or drop a held attachment")
	}
	if _, err := s.author(ctx, request.Actor); err != nil {
		return Attachment{}, err
	}
	a, err := s.Attachment(ctx, request.Ref)
	if err != nil {
		return Attachment{}, err
	}
	if a.State != AttachmentHeld {
		return Attachment{}, refuse(CodeInvalid, "attachment %s is %s, not held", short(a.Ref), a.State)
	}
	return a, nil
}

// Release lets a held attachment through the one check that held it. It is
// still re-encoded; posts that name it show it from then on.
func (s *Service) Release(ctx context.Context, request HeldRequest) (Attachment, error) {
	a, err := s.held(ctx, request)
	if err != nil {
		return Attachment{}, err
	}
	original, err := s.media.Quarantined(a)
	if err != nil {
		return Attachment{}, err
	}
	skip := Skip{Scan: strings.HasPrefix(a.Reason, "signature scan"), Decoder: strings.HasPrefix(a.Reason, "decoder")}
	result, err := s.media.Ingest(ctx, a.Name, original, skip)
	if err != nil {
		return Attachment{}, err
	}
	result.Ref, result.CreatedAt = a.Ref, a.CreatedAt
	if result.State == AttachmentHeld {
		result.Bytes = int64(len(original))
	} else {
		result.Checks = append(result.Checks, "released by "+request.Person)
		if err := s.media.Discard(a); err != nil {
			return Attachment{}, err
		}
	}
	return result, s.attachments.PutAttachment(ctx, result)
}

// Drop deletes a held attachment's original; posts naming it show it as
// removed.
func (s *Service) Drop(ctx context.Context, request HeldRequest) (Attachment, error) {
	a, err := s.held(ctx, request)
	if err != nil {
		return Attachment{}, err
	}
	if err := s.media.Discard(a); err != nil {
		return Attachment{}, err
	}
	a.State, a.Reason = AttachmentDropped, "dropped by "+request.Person
	return a, s.attachments.PutAttachment(ctx, a)
}
