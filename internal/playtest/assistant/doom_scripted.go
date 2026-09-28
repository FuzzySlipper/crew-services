package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

const (
	doomFarTargetDistance      = 15.0
	doomCoarseTurnDegrees      = 10.0
	doomYawCorrectionDegrees   = 3.0
	doomPitchCorrectionDegrees = 3.0
	doomBlockedDisplacement    = 0.05
	doomBlockedForwardCycles   = 3
	doomMovementGroup          = "movement"
	doomLookGroup              = "look"
	doomTriggerGroup           = "trigger"
	doomMoveNone               = "move_none"
	doomMoveForward            = "forward"
	doomMoveBack               = "back"
	doomStrafeLeft             = "strafe_left"
	doomStrafeRight            = "strafe_right"
	doomLookNone               = "look_none"
	doomTurnLeft               = "turn_left"
	doomTurnRight              = "turn_right"
	doomAimLeft                = "aim_left"
	doomAimRight               = "aim_right"
	doomAimUp                  = "aim_up"
	doomAimDown                = "aim_down"
	doomTriggerNone            = "trigger_none"
	doomFire                   = "fire"
)

var doomRecoveryMoves = []string{doomStrafeLeft, doomStrafeRight, doomMoveBack}

// DoomScripted is a deliberately small controller for comparison experiments.
// It uses only product-published combat facts and the ordinary grouped controls.
type DoomScripted struct {
	mu                   sync.Mutex
	blockedForwardCycles int
	recoveryMoveIndex    int
}

// Decide chooses simultaneous movement, look, and trigger controls from the
// current combat observation. It does not infer a route or product state that
// the observation has not published.
func (d *DoomScripted) Decide(ctx context.Context, state State, _ []Tactic) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	combat, err := readDoomCombat(state.Observation)
	if err != nil {
		return Decision{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.observeForwardDisplacement(state, combat); err != nil {
		return Decision{}, err
	}

	choices := map[string]string{
		doomMovementGroup: doomMoveNone,
		doomLookGroup:     doomLookNone,
		doomTriggerGroup:  doomTriggerNone,
	}
	if d.blockedForwardCycles >= doomBlockedForwardCycles && d.recoveryMoveIndex < len(doomRecoveryMoves) {
		choices[doomMovementGroup] = doomRecoveryMoves[d.recoveryMoveIndex]
		d.recoveryMoveIndex++
		if d.recoveryMoveIndex == len(doomRecoveryMoves) {
			d.blockedForwardCycles = 0
			d.recoveryMoveIndex = 0
		}
		return doomDecision(choices), nil
	}

	target := nearestVisibleDoomEnemy(combat.Enemies)
	if target == nil {
		choices[doomMovementGroup] = doomMoveForward
		choices[doomLookGroup] = doomTurnRight
		return doomDecision(choices), nil
	}

	if *target.Distance > doomFarTargetDistance {
		choices[doomMovementGroup] = doomMoveForward
	}
	if math.Abs(*target.BearingDegrees) > doomCoarseTurnDegrees {
		if *target.BearingDegrees > 0 {
			choices[doomLookGroup] = doomTurnRight
		} else {
			choices[doomLookGroup] = doomTurnLeft
		}
		return doomDecision(choices), nil
	}
	if math.Abs(*target.BearingDegrees) > doomYawCorrectionDegrees {
		if *target.BearingDegrees > 0 {
			choices[doomLookGroup] = doomAimRight
		} else {
			choices[doomLookGroup] = doomAimLeft
		}
		return doomDecision(choices), nil
	}
	if math.Abs(*target.AimPitchErrorDegrees) > doomPitchCorrectionDegrees {
		if *target.AimPitchErrorDegrees > 0 {
			choices[doomLookGroup] = doomAimUp
		} else {
			choices[doomLookGroup] = doomAimDown
		}
		return doomDecision(choices), nil
	}
	if *combat.Player.WeaponReady && doomCurrentHitMatchesTarget(combat.Player, *target) {
		choices[doomTriggerGroup] = doomFire
	}
	return doomDecision(choices), nil
}

func doomDecision(choices map[string]string) Decision {
	return Decision{Choice: "continue", Choices: choices, Confidence: 1}
}

func (d *DoomScripted) observeForwardDisplacement(state State, current doomCombatObservation) error {
	if !hasCompositeChoice(state.LastAction, doomMovementGroup, doomMoveForward) {
		return nil
	}
	if state.Previous == nil {
		return errors.New("doom scripted controller needs previous combat.observe after forward movement")
	}
	previous, err := readDoomCombat(*state.Previous)
	if err != nil {
		return fmt.Errorf("previous combat.observe: %w", err)
	}
	dx := *current.Player.Position.X - *previous.Player.Position.X
	dz := *current.Player.Position.Z - *previous.Player.Position.Z
	if math.Hypot(dx, dz) <= doomBlockedDisplacement {
		d.blockedForwardCycles++
		return nil
	}
	d.blockedForwardCycles = 0
	d.recoveryMoveIndex = 0
	return nil
}

func hasCompositeChoice(action, group, choice string) bool {
	for _, part := range strings.Split(action, ";") {
		if part == group+"="+choice {
			return true
		}
	}
	return false
}

func nearestVisibleDoomEnemy(enemies []doomEnemyObservation) *doomEnemyObservation {
	var target *doomEnemyObservation
	for i := range enemies {
		enemy := &enemies[i]
		if *enemy.Health <= 0 || !*enemy.LineOfSight {
			continue
		}
		if target == nil || *enemy.Distance < *target.Distance {
			target = enemy
		}
	}
	return target
}

func doomCurrentHitMatchesTarget(player doomPlayerObservation, target doomEnemyObservation) bool {
	hit := player.AimAssist.Shot.AssistedHit
	if !*player.AimAssist.Active {
		hit = player.AimAssist.Shot.RawHit
	}
	return *hit.Kind == "Entity" && sameDoomID(hit.Entity, target.ID)
}

func sameDoomID(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 || bytes.Equal(bytes.TrimSpace(a), []byte("null")) || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return false
	}
	var left, right any
	leftDecoder := json.NewDecoder(bytes.NewReader(a))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(b))
	rightDecoder.UseNumber()
	return leftDecoder.Decode(&left) == nil && rightDecoder.Decode(&right) == nil && fmt.Sprint(left) == fmt.Sprint(right)
}

type doomCombatObservation struct {
	Player  doomPlayerObservation  `json:"player"`
	Enemies []doomEnemyObservation `json:"enemies"`
}

type doomPlayerObservation struct {
	Position    doomPositionObservation  `json:"position"`
	WeaponReady *bool                    `json:"weaponReady"`
	AimAssist   doomAimAssistObservation `json:"aimAssist"`
}

type doomPositionObservation struct {
	X *float64 `json:"x"`
	Z *float64 `json:"z"`
}

type doomAimAssistObservation struct {
	Active *bool               `json:"active"`
	Shot   doomShotObservation `json:"shot"`
}

type doomShotObservation struct {
	RawHit      doomHitObservation `json:"rawHit"`
	AssistedHit doomHitObservation `json:"assistedHit"`
}

type doomHitObservation struct {
	Kind   *string         `json:"kind"`
	Entity json.RawMessage `json:"entity"`
}

type doomEnemyObservation struct {
	ID                   json.RawMessage `json:"id"`
	Health               *float64        `json:"health"`
	Distance             *float64        `json:"distance"`
	BearingDegrees       *float64        `json:"bearingDegrees"`
	AimPitchErrorDegrees *float64        `json:"aimPitchErrorDegrees"`
	LineOfSight          *bool           `json:"lineOfSight"`
}

func readDoomCombat(observation Observation) (doomCombatObservation, error) {
	raw, ok := observation.Facts["combat.observe"]
	if !ok {
		return doomCombatObservation{}, errors.New("doom scripted controller requires combat.observe fact")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var combat doomCombatObservation
	if err := decoder.Decode(&combat); err != nil {
		return doomCombatObservation{}, fmt.Errorf("invalid combat.observe fact: %w", err)
	}
	if combat.Player.Position.X == nil || combat.Player.Position.Z == nil || combat.Player.WeaponReady == nil || combat.Player.AimAssist.Active == nil || combat.Player.AimAssist.Shot.RawHit.Kind == nil || combat.Player.AimAssist.Shot.AssistedHit.Kind == nil {
		return doomCombatObservation{}, errors.New("combat.observe missing required player combat fact")
	}
	for i, enemy := range combat.Enemies {
		if len(enemy.ID) == 0 || enemy.Health == nil || enemy.Distance == nil || enemy.BearingDegrees == nil || enemy.AimPitchErrorDegrees == nil || enemy.LineOfSight == nil {
			return doomCombatObservation{}, fmt.Errorf("combat.observe missing required enemy fact at index %d", i)
		}
	}
	return combat, nil
}
