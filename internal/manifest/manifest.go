package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Manifest is the small developer-facing contract for a Plinth service.
type Manifest struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Port      int               `json:"port"`
	Replicas  int               `json:"replicas"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   []string          `json:"secrets,omitempty"`
	Resources Resources         `json:"resources"`
}

type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}[a-z0-9]$|^[a-z]$`)

func LoadFile(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return Parse(b)
}

func Parse(data []byte) (Manifest, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Manifest{}, errors.New("manifest is empty")
	}

	var m Manifest
	var err error
	if trimmed[0] == '{' {
		err = json.Unmarshal(trimmed, &m)
	} else {
		m, err = parseYAML(string(trimmed))
	}
	if err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("name must be a DNS-safe service name, got %q", m.Name)
	}
	if strings.TrimSpace(m.Image) == "" {
		return errors.New("image is required")
	}
	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", m.Port)
	}
	if m.Replicas < 0 || m.Replicas > 1000 {
		return fmt.Errorf("replicas must be between 0 and 1000, got %d", m.Replicas)
	}
	for key, value := range m.Env {
		if strings.TrimSpace(key) == "" {
			return errors.New("env keys cannot be empty")
		}
		if strings.ContainsAny(key, "= \t") {
			return fmt.Errorf("invalid env key %q", key)
		}
		if value == "" {
			return fmt.Errorf("env value for %q cannot be empty", key)
		}
	}
	for _, secret := range m.Secrets {
		if strings.TrimSpace(secret) == "" {
			return errors.New("secret names cannot be empty")
		}
	}
	if strings.TrimSpace(m.Resources.CPU) == "" || strings.TrimSpace(m.Resources.Memory) == "" {
		return errors.New("resources.cpu and resources.memory are required")
	}
	return nil
}

func parseYAML(input string) (Manifest, error) {
	var m Manifest
	m.Env = map[string]string{}
	section := ""
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	for lineNumber, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if strings.HasPrefix(line, "-") {
			if section != "secrets" || indent < 2 {
				return Manifest{}, fmt.Errorf("line %d: list item is only valid under secrets", lineNumber+1)
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, "-"))
			if value == "" {
				return Manifest{}, fmt.Errorf("line %d: empty secret name", lineNumber+1)
			}
			m.Secrets = append(m.Secrets, unquote(value))
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return Manifest{}, fmt.Errorf("line %d: expected key: value", lineNumber+1)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if indent == 0 {
			section = ""
			if value == "" {
				switch key {
				case "env", "resources", "secrets":
					section = key
					continue
				default:
					return Manifest{}, fmt.Errorf("line %d: %s needs a value", lineNumber+1, key)
				}
			}
			switch key {
			case "name":
				m.Name = unquote(value)
			case "image":
				m.Image = unquote(value)
			case "port":
				var parseErr error
				m.Port, parseErr = strconv.Atoi(value)
				if parseErr != nil {
					return Manifest{}, fmt.Errorf("line %d: port must be an integer", lineNumber+1)
				}
			case "replicas":
				var parseErr error
				m.Replicas, parseErr = strconv.Atoi(value)
				if parseErr != nil {
					return Manifest{}, fmt.Errorf("line %d: replicas must be an integer", lineNumber+1)
				}
			case "env", "resources", "secrets":
				return Manifest{}, fmt.Errorf("line %d: %s must use nested values", lineNumber+1, key)
			default:
				return Manifest{}, fmt.Errorf("line %d: unknown field %q", lineNumber+1, key)
			}
			continue
		}

		if indent < 2 || section == "" {
			return Manifest{}, fmt.Errorf("line %d: unexpected indentation", lineNumber+1)
		}
		switch section {
		case "env":
			m.Env[key] = unquote(value)
		case "resources":
			switch key {
			case "cpu":
				m.Resources.CPU = unquote(value)
			case "memory":
				m.Resources.Memory = unquote(value)
			default:
				return Manifest{}, fmt.Errorf("line %d: unknown resource %q", lineNumber+1, key)
			}
		case "secrets":
			return Manifest{}, fmt.Errorf("line %d: secrets must be list items", lineNumber+1)
		}
	}
	return m, nil
}

func stripComment(line string) string {
	quoted := false
	for i, r := range line {
		switch r {
		case '\'', '"':
			quoted = !quoted
		case '#':
			if !quoted {
				return line[:i]
			}
		}
	}
	return line
}

func unquote(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}
