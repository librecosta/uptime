package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"

	"go.yaml.in/yaml/v3"
)

// LoadFile validates one YAML document. Relative paths remain relative to the
// process working directory. Only the YAML and active TLS files are read.
func LoadFile(path string) (Config, error) {
	return loadFile(path, false)
}

// LoadServiceFile validates maintenance configuration. Only an explicitly empty
// endpoints sequence is additionally allowed; omitted endpoints remain invalid.
func LoadServiceFile(path string) (Config, error) {
	return loadFile(path, true)
}

func loadFile(path string, maintenance bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// File paths can themselves contain injected secrets; do not wrap PathError.
		return Config{}, fmt.Errorf("configuration: cannot read file")
	}
	return loadMode(data, os.LookupEnv, maintenance)
}

func load(data []byte, lookup envLookup) (Config, error) {
	return loadMode(data, lookup, false)
}

func loadMode(data []byte, lookup envLookup, maintenance bool) (Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return Config{}, fmt.Errorf("configuration: invalid or empty YAML document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration: expected exactly one YAML document")
	}
	var raw rawConfig
	if err := checkNode(doc.Content[0], reflect.TypeFor[rawConfig](), "configuration", make(map[*yaml.Node]bool)); err != nil {
		return Config{}, err
	}
	decoder = yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		// YAML library errors can quote scalar contents, including secrets.
		return Config{}, fmt.Errorf("configuration: invalid YAML field value")
	}
	raw.defaults()
	return normalize(raw, lookup, maintenance)
}

// checkNode enforces the raw schema before decoding can coerce scalars or treat
// null as omission. It also produces safe field/line diagnostics without copying
// YAML values or unknown keys into errors. KnownFields remains enabled above.
func checkNode(n *yaml.Node, typ reflect.Type, field string, visiting map[*yaml.Node]bool) error {
	if visiting[n] {
		return fmt.Errorf("%s: cyclic YAML alias", field)
	}
	visiting[n] = true
	defer delete(visiting, n)
	if n.Kind == yaml.AliasNode {
		return checkNode(n.Alias, typ, field, visiting)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	wrongType := func() error { return fmt.Errorf("%s: unexpected YAML type at line %d", field, n.Line) }
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
			return wrongType()
		}
		fields := make(map[string]reflect.Type)
		if typ.Kind() == reflect.Struct {
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				fields[f.Tag.Get("yaml")] = f.Type
			}
		}
		seen := make(map[string]bool)
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("%s: expected literal string key at line %d", field, key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("%s: duplicate key at line %d", field, key.Line)
			}
			seen[key.Value] = true
			childField := fmt.Sprintf("%s[%d]", field, i/2)
			var childType reflect.Type
			if typ.Kind() == reflect.Struct {
				var ok bool
				childType, ok = fields[key.Value]
				if !ok {
					return fmt.Errorf("%s: unknown field at line %d", field, key.Line)
				}
				childField = field + "." + key.Value
			} else {
				childType = typ.Elem()
			}
			if err := checkNode(value, childType, childField, visiting); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode || n.Tag != "!!seq" {
			return wrongType()
		}
		for i, item := range n.Content {
			if err := checkNode(item, typ.Elem(), fmt.Sprintf("%s[%d]", field, i), visiting); err != nil {
				return err
			}
		}
	default:
		if n.Kind != yaml.ScalarNode {
			return wrongType()
		}
		valid := typ.Kind() == reflect.String && n.Tag == "!!str" ||
			typ.Kind() == reflect.Bool && n.Tag == "!!bool" ||
			typ.Kind() == reflect.Int && n.Tag == "!!int" ||
			typ.Kind() == reflect.Float64 && (n.Tag == "!!int" || n.Tag == "!!float")
		if !valid {
			return wrongType()
		}
	}
	return nil
}
