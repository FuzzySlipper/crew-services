package fabricclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

type Holding struct {
	SeatAddress      string `json:"seat_address"`
	HoldingID        int64  `json:"holding_id"`
	SeatGeneration   int64  `json:"seat_generation"`
	HolderAddress    string `json:"holder_address"`
	HolderAdapterID  string `json:"holder_adapter_id"`
	HolderGeneration int64  `json:"holder_generation"`
	StartedAt        string `json:"started_at"`
	StartNote        string `json:"start_note,omitempty"`
	EndedAt          string `json:"ended_at,omitempty"`
	EndReason        string `json:"end_reason,omitempty"`
	EndNote          string `json:"end_note,omitempty"`
	Stale            bool   `json:"stale,omitempty"`
	StaleReason      string `json:"stale_reason,omitempty"`
}

type SeatText struct {
	SeatAddress    string   `json:"seat_address"`
	Kind           string   `json:"kind"`
	Version        int64    `json:"version"`
	Body           string   `json:"body,omitempty"`
	Length         int      `json:"length"`
	SetAt          string   `json:"set_at"`
	SetByAddress   string   `json:"set_by_address"`
	SetByAdapterID string   `json:"set_by_adapter_id"`
	SetBySeats     []string `json:"set_by_seats"`
	HoldingID      int64    `json:"holding_id,omitempty"`
}

type Seat struct {
	Address        string    `json:"address"`
	Generation     int64     `json:"generation"`
	State          string    `json:"state"`
	Holding        *Holding  `json:"holding,omitempty"`
	CharterVersion int64     `json:"charter_version"`
	CharterSummary string    `json:"charter_summary,omitempty"`
	HandoffVersion int64     `json:"handoff_version"`
	QueuedMail     int       `json:"queued_mail"`
	CreatedAt      string    `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
	Charter        *SeatText `json:"charter,omitempty"`
	Handoff        *SeatText `json:"handoff,omitempty"`
}

// SeatFilter narrows ListSeats; empty fields do not filter.
type SeatFilter struct {
	Scope  string
	Holder string
	State  string
}

// SeatWriteRequest is a seat write made by a session through its adapter.
// Body and ExpectedVersion are used only by the charter and handoff writes.
type SeatWriteRequest struct {
	AdapterID       string `json:"adapter_id"`
	LeaseToken      string `json:"lease_token"`
	OperationID     string `json:"operation_id"`
	SessionAddress  string `json:"session_address"`
	Note            string `json:"note,omitempty"`
	Body            string `json:"body,omitempty"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
}

// SeatWrite names: the seat operations and the two texts.
var SeatWrites = []string{"take", "leave", "relieve", "retire", "charter", "handoff"}

// ListSeats is a read; it never claims or delivers anything.
func (c *Client) ListSeats(ctx context.Context, filter SeatFilter) ([]Seat, error) {
	query := url.Values{}
	for key, value := range map[string]string{"scope": filter.Scope, "holder": filter.Holder, "state": filter.State} {
		if value != "" {
			query.Set(key, value)
		}
	}
	path := "/v1/seats"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var out struct {
		Seats []Seat `json:"seats"`
	}
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out.Seats, err
}

// GetSeat returns one seat with its charter and handoff in full.
func (c *Client) GetSeat(ctx context.Context, seat string) (Seat, error) {
	var out Seat
	err := c.call(ctx, http.MethodGet, "/v1/seats/"+url.PathEscape(seat), nil, &out)
	return out, err
}

// GetSeatText returns one version of a seat's charter or handoff; version 0
// is the current one.
func (c *Client) GetSeatText(ctx context.Context, seat, kind string, version int64) (SeatText, error) {
	path := "/v1/seats/" + url.PathEscape(seat) + "/" + kind
	if version > 0 {
		path += "?version=" + strconv.FormatInt(version, 10)
	}
	var out SeatText
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) ListSeatTexts(ctx context.Context, seat, kind string) ([]SeatText, error) {
	var out struct {
		Versions []SeatText `json:"versions"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/seats/"+url.PathEscape(seat)+"/"+kind+"/versions", nil, &out)
	return out.Versions, err
}

func (c *Client) ListSeatHoldings(ctx context.Context, seat string) ([]Holding, error) {
	var out struct {
		Holdings []Holding `json:"holdings"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/seats/"+url.PathEscape(seat)+"/holdings", nil, &out)
	return out.Holdings, err
}

// SeatWrite makes one seat write: take, leave, relieve or retire, or sets the
// charter or handoff. It returns the fabric's result as it came.
func (c *Client) SeatWrite(ctx context.Context, write, seat string, request SeatWriteRequest) (json.RawMessage, error) {
	method, path := http.MethodPost, "/v1/seats/"+url.PathEscape(seat)+"/"+write
	if write == "charter" || write == "handoff" {
		method = http.MethodPut
	}
	var out json.RawMessage
	err := c.call(ctx, method, path, request, &out)
	return out, err
}

// SeatRoute is one seat a driver's session holds: the driver claims the
// seat's mail at Seat and Generation and delivers it into Holder.
type SeatRoute struct {
	Seat       string
	Generation int64
	Holder     string
}

// HeldSeats lists the seats held through adapterID, by holder address. A
// stale holding is left out: its mail waits until someone relieves it.
func HeldSeats(ctx context.Context, seats interface {
	ListSeats(context.Context, SeatFilter) ([]Seat, error)
}, adapterID string) (map[string][]SeatRoute, error) {
	list, err := seats.ListSeats(ctx, SeatFilter{State: "held"})
	if err != nil {
		return nil, err
	}
	out := map[string][]SeatRoute{}
	for _, s := range list {
		if h := s.Holding; h != nil && !h.Stale && h.HolderAdapterID == adapterID {
			out[h.HolderAddress] = append(out[h.HolderAddress], SeatRoute{Seat: s.Address, Generation: s.Generation, Holder: h.HolderAddress})
		}
	}
	return out, nil
}

// StatusBody lets a loopback API pass a fabric refusal through as it came.
func (e *HTTPError) StatusBody() (int, string) { return e.Status, e.Body }
