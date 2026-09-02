package pipelines

import (
	"encoding/json"
	"fmt"
)

// Spec is the JSON form of a Definition, as stored in pipeline_definitions.spec.
//
// Kept separate from Definition so the wire format and the in-memory model can change
// independently: a stored spec written months ago must still parse after the planner
// grows a field.
type Spec struct {
	Key   string     `json:"key"`
	Name  string     `json:"name"`
	Steps []StepSpec `json:"steps"`
}

type StepSpec struct {
	Key             string            `json:"key"`
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	DependsOn       []string          `json:"depends_on,omitempty"`
	Command         string            `json:"command,omitempty"`
	Image           string            `json:"image,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
	MaxAttempts     int               `json:"max_attempts,omitempty"`
	ContinueOnError bool              `json:"continue_on_error,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
}

// ParseSpec decodes a stored spec and validates the resulting graph.
//
// Validation happens here rather than at use time so a malformed definition is caught
// when a run is requested, with a message naming the problem, instead of producing a run
// whose DAG can never complete.
func ParseSpec(raw []byte) (Definition, error) {
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Definition{}, fmt.Errorf("decode pipeline spec: %w", err)
	}

	def := Definition{Key: spec.Key, Name: spec.Name}
	for _, s := range spec.Steps {
		step := StepDef{
			Key:             s.Key,
			Name:            s.Name,
			Type:            StepType(s.Type),
			DependsOn:       s.DependsOn,
			Command:         s.Command,
			Image:           s.Image,
			TimeoutSeconds:  s.TimeoutSeconds,
			ContinueOnError: s.ContinueOnError,
			Env:             s.Env,
		}
		if s.Name == "" {
			step.Name = s.Key
		}
		if s.MaxAttempts > 0 {
			policy := DefaultRetryPolicy()
			policy.MaxAttempts = s.MaxAttempts
			step.Retry = &policy
		}
		def.Steps = append(def.Steps, step)
	}

	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// MarshalSpec renders a definition back to its stored form.
func MarshalSpec(d Definition) ([]byte, error) {
	spec := Spec{Key: d.Key, Name: d.Name}
	for _, s := range d.Steps {
		out := StepSpec{
			Key: s.Key, Name: s.Name, Type: string(s.Type), DependsOn: s.DependsOn,
			Command: s.Command, Image: s.Image, TimeoutSeconds: s.TimeoutSeconds,
			ContinueOnError: s.ContinueOnError, Env: s.Env,
		}
		if s.Retry != nil {
			out.MaxAttempts = s.Retry.MaxAttempts
		}
		spec.Steps = append(spec.Steps, out)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode pipeline spec: %w", err)
	}
	return b, nil
}
