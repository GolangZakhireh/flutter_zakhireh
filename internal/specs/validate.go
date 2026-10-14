package specs

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxSpecNameLen bounds the logical spec name length.
const MaxSpecNameLen = 128

// ValidationError is an actionable, user-facing validation failure.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Message)
	}
	return e.Message
}

// CheckExtension rejects anything that is not .yaml/.yml.
func CheckExtension(filename string) *ValidationError {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".yaml" && ext != ".yml" {
		return &ValidationError{
			Field:   "file",
			Message: fmt.Sprintf("unsupported extension %q: upload a .yaml or .yml file", filepath.Ext(filename)),
		}
	}
	return nil
}

// ParseAndValidate parses YAML syntax and validates the POP Spec structure.
// It returns the parsed spec so callers can derive the logical name and
// generate the example project without re-parsing.
func ParseAndValidate(data []byte) (*PopSpec, *ValidationError) {
	if len(data) == 0 {
		return nil, &ValidationError{Field: "file", Message: "file is empty"}
	}
	var spec PopSpec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(false)
	if err := dec.Decode(&spec); err != nil {
		return nil, &ValidationError{Field: "yaml", Message: fmt.Sprintf("invalid YAML syntax: %v", err)}
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, &ValidationError{Field: "name", Message: "missing required field 'name': add e.g. 'name: my_app' to the spec"}
	}
	if len(spec.Name) > MaxSpecNameLen {
		return nil, &ValidationError{Field: "name", Message: fmt.Sprintf("field 'name' exceeds %d characters", MaxSpecNameLen)}
	}
	for _, r := range spec.Name {
		if !(r == '_' || r == '-' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return nil, &ValidationError{
				Field:   "name",
				Message: "field 'name' may only contain letters, digits, '_', '-', '.'",
			}
		}
	}
	if spec.Dependencies == nil && spec.DevDependencies == nil {
		return nil, &ValidationError{
			Field:   "dependencies",
			Message: "spec defines no 'dependencies' or 'dev_dependencies': nothing to generate",
		}
	}
	return &spec, nil
}

// SanitizeFilename keeps the original upload name as display metadata only:
// it strips directories, control characters and over-long names so the value
// can never be used for path traversal.
func SanitizeFilename(name string) string {
	base := filepath.Base(name)
	base = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return "upload.yaml"
	}
	if len(base) > 255 {
		ext := filepath.Ext(base)
		base = base[:255-len(ext)] + ext
	}
	return base
}
