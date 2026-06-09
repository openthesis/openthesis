package orchestrator

import (
	"bytes"
	"encoding/json"

	"github.com/openthesis/openthesis/internal/explorer"
)

type parsedLifecycle struct {
	eventType string
	details   map[string]any
}

// decodeStatusDetails unmarshals a JSON payload into a {Status, Details} body
// and appends a parsedLifecycle with the given eventType to results.
// Returns the updated slice.
func decodeStatusDetails(results []parsedLifecycle, eventType string, payload json.RawMessage) []parsedLifecycle {
	var body struct {
		Status  string         `json:"status"`
		Details map[string]any `json:"details"`
	}
	if json.Unmarshal(payload, &body) == nil {
		results = append(results, parsedLifecycle{eventType: eventType, details: body.Details})
	}
	return results
}

// parseLifecycleOutput scans JSONL data for OpenThesis lifecycle events.
func parseLifecycleOutput(data []byte) []parsedLifecycle {
	var results []parsedLifecycle

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}

		if payload, ok := raw["openthesis_setup_complete"]; ok {
			results = decodeStatusDetails(results, "setup_complete", payload)
		}

		if payload, ok := raw["openthesis_send_event"]; ok {
			var body struct {
				EventName string         `json:"event_name"`
				Details   map[string]any `json:"details"`
			}
			if json.Unmarshal(payload, &body) == nil {
				results = append(results, parsedLifecycle{
					eventType: body.EventName,
					details:   body.Details,
				})
			}
		}

		if payload, ok := raw["openthesis_teardown"]; ok {
			results = decodeStatusDetails(results, "teardown", payload)
		}

		if payload, ok := raw["openthesis_prefork"]; ok {
			results = decodeStatusDetails(results, "openthesis_prefork", payload)
		}

		if payload, ok := raw["openthesis_burst_done"]; ok {
			results = decodeStatusDetails(results, "openthesis_burst_done", payload)
		}

		if payload, ok := raw["openthesis_stop_faults"]; ok {
			var body struct {
				DurationSeconds float64 `json:"duration_seconds"`
				Status          string  `json:"status"`
			}
			if json.Unmarshal(payload, &body) == nil {
				results = append(results, parsedLifecycle{
					eventType: "openthesis_stop_faults",
					details:   map[string]any{"duration_seconds": body.DurationSeconds},
				})
			}
		}
	}

	return results
}

type parsedAssertion struct {
	assertType string
	condition  bool
	message    string
	// detailsJSON is the raw Details field from the assertion envelope.
	// Preserved so processWorkerOutput can extract operand values (left_value,
	// right_value) for the live observer without re-parsing the entire line.
	detailsJSON []byte
	// SometimesAll sub-goal data (only set when assert_type == "sometimes_all").
	subGoals       map[string]bool
	satisfiedCount int
	totalCount     int
}

// parseAssertionOutput scans JSONL data for SDK assertions.
// Returns all observed assertions and any violations (failed always, reached unreachable).
func parseAssertionOutput(data []byte) ([]parsedAssertion, []explorer.Violation) {
	var assertions []parsedAssertion
	var violations []explorer.Violation

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		type assertBody struct {
			Hit        bool            `json:"hit"`
			Condition  bool            `json:"condition"`
			Message    string          `json:"message"`
			AssertType string          `json:"assert_type"`
			Details    json.RawMessage `json:"details"`
		}
		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(line, &rawMap); err != nil {
			continue
		}
		payload, ok := rawMap["openthesis_assert"]
		if !ok {
			continue
		}
		var a assertBody
		if err := json.Unmarshal(payload, &a); err != nil {
			continue
		}

		if a.Message == "" {
			continue
		}
		// hit:false events are catalog declarations; skip them for evaluation.
		if !a.Hit {
			continue
		}

		pa := parsedAssertion{
			assertType:  a.AssertType,
			condition:   a.Condition,
			message:     a.Message,
			detailsJSON: []byte(a.Details),
		}

		// Extract SometimesAll sub-goal data from details.
		if a.AssertType == "sometimes_all" && len(a.Details) > 0 {
			var details struct {
				SubGoals       map[string]bool `json:"sub_goals"`
				SatisfiedCount int             `json:"satisfied_count"`
				TotalCount     int             `json:"total_count"`
			}
			if err := json.Unmarshal(a.Details, &details); err == nil {
				pa.subGoals = details.SubGoals
				pa.satisfiedCount = details.SatisfiedCount
				pa.totalCount = details.TotalCount
			}
		}

		// Extract details for all assertions (used for violation context).
		var assertDetails map[string]any
		if len(a.Details) > 0 {
			_ = json.Unmarshal(a.Details, &assertDetails)
		}

		assertions = append(assertions, pa)

		// A violation is an "always" or "always_or_unreachable" assertion that was false.
		if (a.AssertType == "always" || a.AssertType == "always_or_unreachable") && !a.Condition {
			violations = append(violations, explorer.Violation{
				Property: a.Message,
				Message:  a.Message,
				Details:  assertDetails,
			})
		}
		// Unreachable: the SDK only emits this when the code path is actually
		// reached, so every emission is a genuine violation.
		if a.AssertType == "unreachable" {
			violations = append(violations, explorer.Violation{
				Property: a.Message,
				Message:  a.Message,
				Details:  assertDetails,
			})
		}
		// EverSince: a monotonic property. The SDK emits effectiveCondition =
		// condition || !armed, so a false emission means the property was
		// previously true (armed) and is now false - a genuine violation.
		if a.AssertType == "ever_since" && !a.Condition {
			violations = append(violations, explorer.Violation{
				Property: a.Message,
				Message:  a.Message,
				Details:  assertDetails,
			})
		}
	}

	return assertions, violations
}

type parsedGuidance struct {
	guidanceType string
	name         string
	value        int64
}

// parseGuidanceOutput scans JSONL data for IJON-style guidance signals
// (MaximizeInt, Explore). These come from the sdk/go/guidance package.
func parseGuidanceOutput(data []byte) []parsedGuidance {
	var results []parsedGuidance

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(line, &rawMap); err != nil {
			continue
		}
		payload, ok := rawMap["openthesis_guidance"]
		if !ok {
			continue
		}

		var g struct {
			GuidanceType string `json:"guidance_type"`
			Name         string `json:"name"`
			Value        int64  `json:"value"`
		}
		if err := json.Unmarshal(payload, &g); err != nil {
			continue
		}

		if g.Name == "" {
			continue
		}

		results = append(results, parsedGuidance{
			guidanceType: g.GuidanceType,
			name:         g.Name,
			value:        g.Value,
		})
	}

	return results
}

type parsedRandomChoice struct {
	chosenIndex  int
	totalChoices int
	valueType    string
}

// parseRandomChoiceOutput scans JSONL data for openthesis_random_choice events
// emitted by sdk/go/random.Choose(). These events let the orchestrator track
// which branch of a Choose() call was taken and guide exploration toward novel branches.
func parseRandomChoiceOutput(data []byte) []parsedRandomChoice {
	var results []parsedRandomChoice

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(line, &rawMap); err != nil {
			continue
		}
		payload, ok := rawMap["openthesis_random_choice"]
		if !ok {
			continue
		}

		var rc struct {
			ChosenIndex  int    `json:"chosen_index"`
			TotalChoices int    `json:"total_choices"`
			ValueType    string `json:"value_type"`
		}
		if err := json.Unmarshal(payload, &rc); err != nil {
			continue
		}

		if rc.TotalChoices == 0 {
			continue
		}

		results = append(results, parsedRandomChoice{
			chosenIndex:  rc.ChosenIndex,
			totalChoices: rc.TotalChoices,
			valueType:    rc.ValueType,
		})
	}

	return results
}

var strategyNames = map[string]explorer.Strategy{
	"breadth": explorer.StrategyBreadthFirst,
	"depth":   explorer.StrategyDepthFirst,
	"mcts":    explorer.StrategyMCTS,
}

func parseStrategy(s string) explorer.Strategy {
	if strat, ok := strategyNames[s]; ok {
		return strat
	}
	return explorer.StrategyCoverageGuided
}
