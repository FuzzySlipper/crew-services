package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/store"
)

type seatRow struct {
	address        string
	generation     int64
	retired        bool
	current        sql.NullInt64
	lastHolding    int64
	charterVersion int64
	handoffVersion int64
	createdAt      time.Time
	createdBy      string
}

func (r seatRow) textVersion(kind string) int64 {
	if kind == store.SeatTextCharter {
		return r.charterVersion
	}
	return r.handoffVersion
}

const seatColumns = `address, generation, retired, current_holding_id, last_holding_id, charter_version, handoff_version, created_at, created_by`

func scanSeatRow(row scanner) (seatRow, error) {
	var value seatRow
	var retired int
	var created string
	if err := row.Scan(&value.address, &value.generation, &retired, &value.current, &value.lastHolding, &value.charterVersion, &value.handoffVersion, &created, &value.createdBy); err != nil {
		return seatRow{}, err
	}
	value.retired = retired == 1
	var err error
	value.createdAt, err = parseTimestamp(created)
	return value, err
}

func readSeatRow(ctx context.Context, q queryer, address string) (seatRow, bool, error) {
	value, err := scanSeatRow(q.QueryRowContext(ctx, `SELECT `+seatColumns+` FROM seats WHERE address = ?`, address))
	if errors.Is(err, sql.ErrNoRows) {
		return seatRow{}, false, nil
	}
	if err != nil {
		return seatRow{}, false, fmt.Errorf("read seat: %w", err)
	}
	return value, true, nil
}

const holdingColumns = `seat_address, holding_id, seat_generation, holder_address, holder_adapter_id, holder_generation, started_at, start_note, ended_at, end_reason, end_note, relieved_by`

func scanHolding(row scanner) (store.Holding, error) {
	var value store.Holding
	var started string
	var ended sql.NullString
	var relieved sql.NullInt64
	if err := row.Scan(&value.SeatAddress, &value.HoldingID, &value.SeatGeneration, &value.HolderAddress, &value.HolderAdapterID, &value.HolderGeneration, &started, &value.StartNote, &ended, &value.EndReason, &value.EndNote, &relieved); err != nil {
		return store.Holding{}, err
	}
	var err error
	if value.StartedAt, err = parseTimestamp(started); err != nil {
		return store.Holding{}, err
	}
	if ended.Valid {
		parsed, err := parseTimestamp(ended.String)
		if err != nil {
			return store.Holding{}, err
		}
		value.EndedAt = &parsed
	}
	value.RelievedBy = relieved.Int64
	return value, nil
}

func readHolding(ctx context.Context, q queryer, seat string, id int64) (store.Holding, error) {
	value, err := scanHolding(q.QueryRowContext(ctx, `SELECT `+holdingColumns+` FROM seat_holdings WHERE seat_address = ? AND holding_id = ?`, seat, id))
	if err != nil {
		return store.Holding{}, fmt.Errorf("read seat holding: %w", err)
	}
	return value, nil
}

// markStale fills the computed stale fields of a current holding. A holding
// is stale when its holder's session binding is gone or now names another
// native session, or when the adapter owning it has no live lease.
func markStale(ctx context.Context, q queryer, now time.Time, holding *store.Holding) error {
	if holding.EndedAt != nil {
		return nil
	}
	binding, found, err := readBinding(ctx, q, holding.HolderAddress)
	if err != nil {
		return err
	}
	switch {
	case !found || !binding.Bound:
		holding.Stale, holding.StaleReason = true, "holder_unbound"
	case binding.Generation != holding.HolderGeneration || binding.AdapterID != holding.HolderAdapterID:
		holding.Stale, holding.StaleReason = true, "holder_rebound"
	default:
		var expires string
		err := q.QueryRowContext(ctx, `SELECT expires_at FROM adapter_leases WHERE adapter_id = ?`, binding.AdapterID).Scan(&expires)
		if errors.Is(err, sql.ErrNoRows) {
			holding.Stale, holding.StaleReason = true, "adapter_lease_expired"
			return nil
		}
		if err != nil {
			return fmt.Errorf("read holder adapter lease: %w", err)
		}
		expiry, err := parseTimestamp(expires)
		if err != nil {
			return err
		}
		if !expiry.After(now.UTC()) {
			holding.Stale, holding.StaleReason, holding.StaleSince = true, "adapter_lease_expired", &expiry
		}
	}
	return nil
}

func scanSeatText(row scanner, withBody bool) (store.SeatText, error) {
	var value store.SeatText
	var setAt, seats string
	var holding sql.NullInt64
	if err := row.Scan(&value.SeatAddress, &value.Kind, &value.Version, &value.Body, &setAt, &value.SetByAddress, &value.SetByAdapterID, &seats, &holding); err != nil {
		return store.SeatText{}, err
	}
	value.Length = len(value.Body)
	if !withBody {
		value.Body = ""
	}
	value.HoldingID = holding.Int64
	var err error
	if value.SetAt, err = parseTimestamp(setAt); err != nil {
		return store.SeatText{}, err
	}
	if err := json.Unmarshal([]byte(seats), &value.SetBySeats); err != nil {
		return store.SeatText{}, fmt.Errorf("decode seat text setter seats: %w", err)
	}
	if value.SetBySeats == nil {
		value.SetBySeats = []string{}
	}
	return value, nil
}

const seatTextColumns = `seat_address, kind, version, body, set_at, set_by_address, set_by_adapter_id, set_by_seats_json, holding_id`

func readSeatText(ctx context.Context, q queryer, seat, kind string, version int64) (store.SeatText, bool, error) {
	value, err := scanSeatText(q.QueryRowContext(ctx, `SELECT `+seatTextColumns+` FROM seat_texts WHERE seat_address = ? AND kind = ? AND version = ?`, seat, kind, version), true)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SeatText{}, false, nil
	}
	if err != nil {
		return store.SeatText{}, false, fmt.Errorf("read seat text: %w", err)
	}
	return value, true, nil
}

// seatView builds the read model. full adds the current charter and handoff;
// otherwise only the charter's first line is included.
func seatView(ctx context.Context, q queryer, now time.Time, row seatRow, full bool) (store.Seat, error) {
	view := store.Seat{Address: row.address, Generation: row.generation, State: store.SeatVacant, CharterVersion: row.charterVersion, HandoffVersion: row.handoffVersion, CreatedAt: row.createdAt, CreatedBy: row.createdBy}
	if row.retired {
		view.State = store.SeatRetired
	}
	if row.current.Valid {
		holding, err := readHolding(ctx, q, row.address, row.current.Int64)
		if err != nil {
			return store.Seat{}, err
		}
		if err := markStale(ctx, q, now, &holding); err != nil {
			return store.Seat{}, err
		}
		view.Holding = &holding
		view.State = store.SeatHeld
		if holding.Stale {
			view.State = store.SeatStale
		}
	}
	for _, kind := range []string{store.SeatTextCharter, store.SeatTextHandoff} {
		version := row.textVersion(kind)
		if version == 0 {
			continue
		}
		text, found, err := readSeatText(ctx, q, row.address, kind, version)
		if err != nil {
			return store.Seat{}, err
		}
		if !found {
			continue
		}
		if kind == store.SeatTextCharter {
			view.CharterSummary, _, _ = strings.Cut(strings.TrimSpace(text.Body), "\n")
		}
		if full {
			if kind == store.SeatTextCharter {
				view.Charter = &text
			} else {
				view.Handoff = &text
			}
		}
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE recipient_address = ? AND recipient_generation = ? AND state IN ('queued','claimed','dispatching')`, row.address, row.generation).Scan(&view.QueuedMail); err != nil {
		return store.Seat{}, fmt.Errorf("count seat mail: %w", err)
	}
	return view, nil
}

func (s *Store) LookupSeatOperation(ctx context.Context, adapterID, operationID string) (store.SeatOperationLookup, error) {
	return lookupSeatReceipt(ctx, s.db, adapterID, operationID)
}

func lookupSeatReceipt(ctx context.Context, q queryer, adapterID, operationID string) (store.SeatOperationLookup, error) {
	var fingerprint, encoded string
	err := q.QueryRowContext(ctx, `SELECT fingerprint, result_json FROM seat_operation_receipts WHERE adapter_id = ? AND operation_id = ?`, adapterID, operationID).Scan(&fingerprint, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SeatOperationLookup{}, nil
	}
	if err != nil {
		return store.SeatOperationLookup{}, fmt.Errorf("read seat operation receipt: %w", err)
	}
	var result store.SeatOperationResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return store.SeatOperationLookup{}, fmt.Errorf("decode seat operation receipt: %w", err)
	}
	return store.SeatOperationLookup{Found: true, Fingerprint: fingerprint, Result: result}, nil
}

// SeatOperation owns every seat write. Like delivery operations, an exact
// retry replays its receipt before any lease, binding or seat check.
func (s *Store) SeatOperation(ctx context.Context, now time.Time, r store.SeatOperationRequest) (store.SeatOperationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("begin seat operation: %w", err)
	}
	defer tx.Rollback()
	if old, err := lookupSeatReceipt(ctx, tx, r.AdapterID, r.OperationID); err != nil {
		return store.SeatOperationResult{}, err
	} else if old.Found {
		if old.Fingerprint != r.Fingerprint {
			return store.SeatOperationResult{}, store.ErrOperationConflict
		}
		old.Result.Replayed = true
		return old.Result, nil
	}
	if _, err := currentLease(ctx, tx, now, r.AdapterID, r.LeaseToken); err != nil {
		return store.SeatOperationResult{}, err
	}
	session, found, err := readBinding(ctx, tx, r.SessionAddress)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if !found {
		return store.SeatOperationResult{}, store.ErrNotFound
	}
	if !session.Bound {
		return store.SeatOperationResult{}, store.ErrNotBound
	}
	if session.AdapterID != r.AdapterID {
		return store.SeatOperationResult{}, store.ErrAdapterMismatch
	}
	op := seatOp{tx: tx, now: now.UTC(), r: r, session: session}
	var result store.SeatOperationResult
	switch r.Kind {
	case "take":
		result, err = op.take(ctx)
	case "leave":
		result, err = op.leave(ctx)
	case "relieve":
		result, err = op.relieve(ctx)
	case "set_text":
		result, err = op.setText(ctx)
	case "retire":
		result, err = op.retire(ctx)
	default:
		err = fmt.Errorf("unknown seat operation %q", r.Kind)
	}
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("encode seat operation receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO seat_operation_receipts (adapter_id, operation_id, kind, fingerprint, result_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`, r.AdapterID, r.OperationID, r.Kind, r.Fingerprint, string(encoded), timestamp(now)); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("record seat operation receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("commit seat operation: %w", err)
	}
	return result, nil
}

type seatOp struct {
	tx      *sql.Tx
	now     time.Time
	r       store.SeatOperationRequest
	session store.Binding
}

// openSeat returns the seat, creating it vacant if it does not exist and
// recreating it with the next generation and empty texts if it was retired.
func (o seatOp) openSeat(ctx context.Context) (seatRow, error) {
	row, found, err := readSeatRow(ctx, o.tx, o.r.SeatAddress)
	if err != nil {
		return seatRow{}, err
	}
	if !found {
		if _, err := o.tx.ExecContext(ctx, `INSERT INTO seats (address, generation, created_at, created_by, updated_at) VALUES (?, 1, ?, ?, ?)`, o.r.SeatAddress, timestamp(o.now), o.r.SessionAddress, timestamp(o.now)); err != nil {
			return seatRow{}, fmt.Errorf("create seat: %w", err)
		}
	} else if row.retired {
		if _, err := o.tx.ExecContext(ctx, `UPDATE seats SET generation = generation + 1, retired = 0, charter_version = 0, handoff_version = 0, created_at = ?, created_by = ?, updated_at = ? WHERE address = ?`, timestamp(o.now), o.r.SessionAddress, timestamp(o.now), o.r.SeatAddress); err != nil {
			return seatRow{}, fmt.Errorf("recreate retired seat: %w", err)
		}
	} else {
		return row, nil
	}
	row, _, err = readSeatRow(ctx, o.tx, o.r.SeatAddress)
	return row, err
}

func (o seatOp) existingSeat(ctx context.Context) (seatRow, error) {
	row, found, err := readSeatRow(ctx, o.tx, o.r.SeatAddress)
	if err != nil {
		return seatRow{}, err
	}
	if !found {
		return seatRow{}, store.ErrSeatNotFound
	}
	return row, nil
}

func (o seatOp) heldError(ctx context.Context, row seatRow) error {
	holding, err := readHolding(ctx, o.tx, row.address, row.current.Int64)
	if err != nil {
		return err
	}
	if err := markStale(ctx, o.tx, o.now, &holding); err != nil {
		return err
	}
	return &store.SeatHeldError{Holding: holding}
}

// startHolding makes the caller the seat's holder and releases any seat-mail
// claim made under the previous holding, so it is claimed again by the new one.
func (o seatOp) startHolding(ctx context.Context, row seatRow) (store.Holding, error) {
	id := row.lastHolding + 1
	if _, err := o.tx.ExecContext(ctx, `INSERT INTO seat_holdings (seat_address, holding_id, seat_generation, holder_address, holder_adapter_id, holder_generation, started_at, start_note) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, row.address, id, row.generation, o.session.Address, o.session.AdapterID, o.session.Generation, timestamp(o.now), o.r.Note); err != nil {
		return store.Holding{}, fmt.Errorf("start seat holding: %w", err)
	}
	if _, err := o.tx.ExecContext(ctx, `UPDATE seats SET current_holding_id = ?, last_holding_id = ?, updated_at = ? WHERE address = ?`, id, id, timestamp(o.now), row.address); err != nil {
		return store.Holding{}, fmt.Errorf("record seat holder: %w", err)
	}
	if err := o.releaseClaims(ctx, row); err != nil {
		return store.Holding{}, err
	}
	return readHolding(ctx, o.tx, row.address, id)
}

func (o seatOp) endHolding(ctx context.Context, row seatRow, reason, note string, relievedBy int64) (store.Holding, error) {
	if _, err := o.tx.ExecContext(ctx, `UPDATE seat_holdings SET ended_at = ?, end_reason = ?, end_note = ?, relieved_by = NULLIF(?, 0) WHERE seat_address = ? AND holding_id = ? AND ended_at IS NULL`, timestamp(o.now), reason, note, relievedBy, row.address, row.current.Int64); err != nil {
		return store.Holding{}, fmt.Errorf("end seat holding: %w", err)
	}
	if _, err := o.tx.ExecContext(ctx, `UPDATE seats SET current_holding_id = NULL, updated_at = ? WHERE address = ?`, timestamp(o.now), row.address); err != nil {
		return store.Holding{}, fmt.Errorf("vacate seat: %w", err)
	}
	if err := o.releaseClaims(ctx, row); err != nil {
		return store.Holding{}, err
	}
	return readHolding(ctx, o.tx, row.address, row.current.Int64)
}

// releaseClaims returns pre-dispatch seat-mail claims to the queue. Work
// already dispatching is reconciled by the adapter that began it.
func (o seatOp) releaseClaims(ctx context.Context, row seatRow) error {
	if _, err := o.tx.ExecContext(ctx, `UPDATE deliveries SET state='queued', claim_owner_adapter_id='', claim_owner_instance_id='', claim_token='', claim_expires_at=NULL, claimed_at=NULL, dispatch_action='' WHERE recipient_address = ? AND state = 'claimed'`, row.address); err != nil {
		return fmt.Errorf("release seat mail claims: %w", err)
	}
	return nil
}

func (o seatOp) previousHolding(ctx context.Context, row seatRow, current int64) (*store.Holding, error) {
	if current <= 1 {
		return nil, nil
	}
	holding, err := readHolding(ctx, o.tx, row.address, current-1)
	if err != nil {
		return nil, err
	}
	return &holding, nil
}

func (o seatOp) result(ctx context.Context, holding *store.Holding, previous *store.Holding) (store.SeatOperationResult, error) {
	row, err := o.existingSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	view, err := seatView(ctx, o.tx, o.now, row, true)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	return store.SeatOperationResult{Seat: view, Holding: holding, PreviousHolding: previous}, nil
}

func (o seatOp) take(ctx context.Context) (store.SeatOperationResult, error) {
	row, err := o.openSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if row.current.Valid {
		return store.SeatOperationResult{}, o.heldError(ctx, row)
	}
	holding, err := o.startHolding(ctx, row)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	previous, err := o.previousHolding(ctx, row, holding.HoldingID)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	return o.result(ctx, &holding, previous)
}

func (o seatOp) leave(ctx context.Context) (store.SeatOperationResult, error) {
	row, err := o.existingSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if !row.current.Valid {
		return store.SeatOperationResult{}, store.ErrNotHolder
	}
	current, err := readHolding(ctx, o.tx, row.address, row.current.Int64)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if current.HolderAddress != o.session.Address {
		return store.SeatOperationResult{}, store.ErrNotHolder
	}
	var found int
	err = o.tx.QueryRowContext(ctx, `SELECT 1 FROM seat_texts WHERE seat_address = ? AND kind = ? AND holding_id = ? LIMIT 1`, row.address, store.SeatTextHandoff, current.HoldingID).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.SeatOperationResult{}, fmt.Errorf("check handoff: %w", err)
	}
	reason := store.HoldingLeft
	if errors.Is(err, sql.ErrNoRows) {
		reason = store.HoldingLeftWithoutHandoff
	}
	ended, err := o.endHolding(ctx, row, reason, o.r.Note, 0)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	return o.result(ctx, nil, &ended)
}

func (o seatOp) relieve(ctx context.Context) (store.SeatOperationResult, error) {
	row, err := o.existingSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if !row.current.Valid {
		return store.SeatOperationResult{}, store.ErrSeatVacant
	}
	current, err := readHolding(ctx, o.tx, row.address, row.current.Int64)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if current.HolderAddress == o.session.Address {
		return store.SeatOperationResult{}, o.heldError(ctx, row)
	}
	ended, err := o.endHolding(ctx, row, store.HoldingRelieved, "", row.lastHolding+1)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	row.current = sql.NullInt64{}
	holding, err := o.startHolding(ctx, row)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	result, err := o.result(ctx, &holding, &ended)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	result.NoticeMessageID, err = o.notice(ctx, ended.HolderAddress, fmt.Sprintf("seat-relief/%s/%d", row.address, ended.HoldingID), store.ReliefNoticeBody(row.address, holding, o.r.Note))
	return result, err
}

func (o seatOp) setText(ctx context.Context) (store.SeatOperationResult, error) {
	row, err := o.openSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	kind := o.r.TextKind
	current := row.textVersion(kind)
	if o.r.ExpectedVersion != nil && *o.r.ExpectedVersion != current {
		return store.SeatOperationResult{}, &store.VersionConflictError{Current: current}
	}
	var issued int64
	if err := o.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM seat_texts WHERE seat_address = ? AND kind = ?`, row.address, kind).Scan(&issued); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("read seat text versions: %w", err)
	}
	version := issued + 1
	seats, err := heldSeats(ctx, o.tx, o.session.Address)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	encoded, err := json.Marshal(seats)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if _, err := o.tx.ExecContext(ctx, `INSERT INTO seat_texts (seat_address, kind, version, body, set_at, set_by_address, set_by_adapter_id, set_by_seats_json, holding_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, row.address, kind, version, o.r.Body, timestamp(o.now), o.session.Address, o.session.AdapterID, string(encoded), row.current); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("insert seat text: %w", err)
	}
	column := "charter_version"
	if kind == store.SeatTextHandoff {
		column = "handoff_version"
	}
	if _, err := o.tx.ExecContext(ctx, `UPDATE seats SET `+column+` = ?, updated_at = ? WHERE address = ?`, version, timestamp(o.now), row.address); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("record seat text version: %w", err)
	}
	if o.r.KeepVersions > 0 {
		if _, err := o.tx.ExecContext(ctx, `DELETE FROM seat_texts WHERE seat_address = ? AND kind = ? AND version <= ?`, row.address, kind, version-int64(o.r.KeepVersions)); err != nil {
			return store.SeatOperationResult{}, fmt.Errorf("prune seat text versions: %w", err)
		}
	}
	text, _, err := readSeatText(ctx, o.tx, row.address, kind, version)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	var holding *store.Holding
	if row.current.Valid {
		value, err := readHolding(ctx, o.tx, row.address, row.current.Int64)
		if err != nil {
			return store.SeatOperationResult{}, err
		}
		holding = &value
	}
	result, err := o.result(ctx, holding, nil)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	result.Text = &text
	if kind == store.SeatTextCharter && holding != nil && holding.HolderAddress != o.session.Address {
		result.NoticeMessageID, err = o.notice(ctx, holding.HolderAddress, fmt.Sprintf("seat-charter/%s/%d", row.address, version), store.CharterNoticeBody(row.address, text))
	}
	return result, err
}

func (o seatOp) retire(ctx context.Context) (store.SeatOperationResult, error) {
	row, err := o.existingSeat(ctx)
	if err != nil {
		return store.SeatOperationResult{}, err
	}
	if row.retired {
		return store.SeatOperationResult{}, store.ErrSeatRetired
	}
	var ended *store.Holding
	if row.current.Valid {
		value, err := o.endHolding(ctx, row, store.HoldingRetired, o.r.Note, 0)
		if err != nil {
			return store.SeatOperationResult{}, err
		}
		ended = &value
	}
	if _, err := o.tx.ExecContext(ctx, `UPDATE seats SET retired = 1, current_holding_id = NULL, updated_at = ? WHERE address = ?`, timestamp(o.now), row.address); err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("retire seat: %w", err)
	}
	rows, err := o.tx.QueryContext(ctx, `SELECT delivery_id FROM deliveries WHERE recipient_address = ? AND state IN ('queued','claimed')`, row.address)
	if err != nil {
		return store.SeatOperationResult{}, fmt.Errorf("list seat mail to fail: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return store.SeatOperationResult{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := terminalize(ctx, o.tx, o.now, id, "failed", "seat_retired"); err != nil {
			return store.SeatOperationResult{}, fmt.Errorf("fail retired seat mail: %w", err)
		}
	}
	return o.result(ctx, nil, ended)
}

// notice queues a service-owned message to a session address. It sends
// nothing if that address is no longer bound.
func (o seatOp) notice(ctx context.Context, recipient, operationID, body string) (string, error) {
	binding, found, err := readBinding(ctx, o.tx, recipient)
	if err != nil {
		return "", err
	}
	if !found || !binding.Bound || o.r.NoticeMessageID == "" {
		return "", nil
	}
	if _, err := o.tx.ExecContext(ctx, `INSERT INTO messages (message_id, producer_id, operation_id, request_fingerprint, sender_address, recipient_address, body, correlation_id, reply_to_message_id, activation_policy, created_at, expires_at, sender_generation, recipient_generation) VALUES (?, ?, ?, '', ?, ?, ?, '', '', ?, ?, ?, 0, ?)`, o.r.NoticeMessageID, store.SeatNoticeSender, operationID, store.SeatNoticeSender, recipient, body, store.PolicyNextTurn, timestamp(o.now), timestamp(o.r.NoticeExpiresAt), binding.Generation); err != nil {
		return "", fmt.Errorf("insert seat notice: %w", err)
	}
	if _, err := o.tx.ExecContext(ctx, `INSERT INTO deliveries (delivery_id, message_id, recipient_address, recipient_generation, state, created_at, expires_at) VALUES (?, ?, ?, ?, 'queued', ?, ?)`, o.r.NoticeDeliveryID, o.r.NoticeMessageID, recipient, binding.Generation, timestamp(o.now), timestamp(o.r.NoticeExpiresAt)); err != nil {
		return "", fmt.Errorf("queue seat notice: %w", err)
	}
	return o.r.NoticeMessageID, nil
}

func heldSeats(ctx context.Context, q queryer, session string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT seat_address FROM seat_holdings WHERE holder_address = ? AND ended_at IS NULL ORDER BY seat_address`, session)
	if err != nil {
		return nil, fmt.Errorf("list held seats: %w", err)
	}
	defer rows.Close()
	seats := make([]string, 0)
	for rows.Next() {
		var seat string
		if err := rows.Scan(&seat); err != nil {
			return nil, err
		}
		seats = append(seats, seat)
	}
	return seats, rows.Err()
}

// currentHolding returns the seat's current holding, or nil if it is vacant.
func currentHolding(ctx context.Context, q queryer, address string) (seatRow, *store.Holding, error) {
	row, found, err := readSeatRow(ctx, q, address)
	if err != nil {
		return seatRow{}, nil, err
	}
	if !found {
		return seatRow{}, nil, store.ErrSeatNotFound
	}
	if row.retired {
		return row, nil, store.ErrSeatRetired
	}
	if !row.current.Valid {
		return row, nil, nil
	}
	holding, err := readHolding(ctx, q, address, row.current.Int64)
	if err != nil {
		return seatRow{}, nil, err
	}
	return row, &holding, nil
}

func (s *Store) ListSeats(ctx context.Context, now time.Time, filter store.SeatFilter) ([]store.Seat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+seatColumns+` FROM seats ORDER BY address`)
	if err != nil {
		return nil, fmt.Errorf("list seats: %w", err)
	}
	var seats []seatRow
	for rows.Next() {
		row, err := scanSeatRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if filter.Scope != "" && !strings.HasSuffix(row.address, "@"+filter.Scope) {
			continue
		}
		seats = append(seats, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	views := make([]store.Seat, 0, len(seats))
	for _, row := range seats {
		view, err := seatView(ctx, s.db, now, row, false)
		if err != nil {
			return nil, err
		}
		if filter.Holder != "" && (view.Holding == nil || view.Holding.HolderAddress != filter.Holder) {
			continue
		}
		if filter.State != "" && view.State != filter.State {
			continue
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) GetSeat(ctx context.Context, now time.Time, address string) (store.Seat, error) {
	row, found, err := readSeatRow(ctx, s.db, address)
	if err != nil {
		return store.Seat{}, err
	}
	if !found {
		return store.Seat{}, store.ErrSeatNotFound
	}
	return seatView(ctx, s.db, now, row, true)
}

// GetSeatText returns one version; version 0 means the current one.
func (s *Store) GetSeatText(ctx context.Context, address, kind string, version int64) (store.SeatText, error) {
	row, found, err := readSeatRow(ctx, s.db, address)
	if err != nil {
		return store.SeatText{}, err
	}
	if !found {
		return store.SeatText{}, store.ErrSeatNotFound
	}
	if version == 0 {
		version = row.textVersion(kind)
		if version == 0 {
			return store.SeatText{}, store.ErrVersionNotFound
		}
	}
	text, found, err := readSeatText(ctx, s.db, address, kind, version)
	if err != nil {
		return store.SeatText{}, err
	}
	if found {
		return text, nil
	}
	var issued int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM seat_texts WHERE seat_address = ? AND kind = ?`, address, kind).Scan(&issued); err != nil {
		return store.SeatText{}, fmt.Errorf("read seat text versions: %w", err)
	}
	if version > 0 && version <= issued {
		return store.SeatText{}, store.ErrVersionPruned
	}
	return store.SeatText{}, store.ErrVersionNotFound
}

// ListSeatTexts returns version metadata, newest first, without bodies.
func (s *Store) ListSeatTexts(ctx context.Context, address, kind string) ([]store.SeatText, error) {
	if _, found, err := readSeatRow(ctx, s.db, address); err != nil {
		return nil, err
	} else if !found {
		return nil, store.ErrSeatNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+seatTextColumns+` FROM seat_texts WHERE seat_address = ? AND kind = ? ORDER BY version DESC`, address, kind)
	if err != nil {
		return nil, fmt.Errorf("list seat texts: %w", err)
	}
	defer rows.Close()
	values := make([]store.SeatText, 0)
	for rows.Next() {
		value, err := scanSeatText(rows, false)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) ListSeatHoldings(ctx context.Context, now time.Time, address string) ([]store.Holding, error) {
	if _, found, err := readSeatRow(ctx, s.db, address); err != nil {
		return nil, err
	} else if !found {
		return nil, store.ErrSeatNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+holdingColumns+` FROM seat_holdings WHERE seat_address = ? ORDER BY holding_id DESC`, address)
	if err != nil {
		return nil, fmt.Errorf("list seat holdings: %w", err)
	}
	values := make([]store.Holding, 0)
	for rows.Next() {
		value, err := scanHolding(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range values {
		if err := markStale(ctx, s.db, now, &values[i]); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// PruneSeatOperations deletes at most limit seat operation receipts recorded
// before cutoff.
func (s *Store) PruneSeatOperations(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM seat_operation_receipts WHERE rowid IN (SELECT rowid FROM seat_operation_receipts WHERE created_at < ? LIMIT ?)`, timestamp(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("prune seat operation receipts: %w", err)
	}
	return result.RowsAffected()
}
