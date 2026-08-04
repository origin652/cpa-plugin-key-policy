package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cpa-key-policy/internal/migration"
	"cpa-key-policy/internal/nativeaccess"
	"cpa-key-policy/internal/policy"
	"gopkg.in/yaml.v3"
)

func main() {
	var legacyState string
	var sourceConfig string
	var outputConfig string
	var outputPolicy string
	var force bool
	flag.StringVar(&legacyState, "legacy-state", "", "legacy cpa-key-policy state JSON")
	flag.StringVar(&sourceConfig, "config", "", "source CPA config.yaml")
	flag.StringVar(&outputConfig, "output-config", "", "staged CPA config.yaml")
	flag.StringVar(&outputPolicy, "output-policy", "", "staged native policy JSON")
	flag.BoolVar(&force, "force", false, "replace staged output files")
	flag.Parse()

	if err := run(legacyState, sourceConfig, outputConfig, outputPolicy, force); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
}

func run(legacyState, sourceConfig, outputConfig, outputPolicy string, force bool) error {
	for name, value := range map[string]string{
		"legacy-state":  legacyState,
		"config":        sourceConfig,
		"output-config": outputConfig,
		"output-policy": outputPolicy,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if samePath(sourceConfig, outputConfig) || samePath(legacyState, outputPolicy) {
		return errors.New("outputs must be staged files, not source files")
	}
	if !force {
		for _, output := range []string{outputConfig, outputPolicy} {
			if _, err := os.Stat(output); err == nil {
				return fmt.Errorf("output already exists: %s", output)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}

	legacy, err := policy.LoadState(legacyState)
	if err != nil {
		return fmt.Errorf("load legacy state: %w", err)
	}
	keys, policies, err := migration.Plan(legacy)
	if err != nil {
		return err
	}
	rawConfig, err := os.ReadFile(sourceConfig)
	if err != nil {
		return err
	}
	stagedConfig, existing, err := replaceAPIKeys(rawConfig, keys)
	if err != nil {
		return err
	}
	stagedConfig, err = enableNativeAccess(stagedConfig)
	if err != nil {
		return err
	}
	migratedHashes := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		migratedHashes[nativeaccess.HashKey(key)] = struct{}{}
	}
	for _, key := range existing {
		if _, covered := migratedHashes[nativeaccess.HashKey(key)]; !covered {
			return errors.New("source config contains a native API key without a migrated policy")
		}
	}
	if err := atomicWrite(outputConfig, stagedConfig, 0o600); err != nil {
		return err
	}
	if force {
		_ = os.Remove(outputPolicy)
	}
	store, err := nativeaccess.New(outputConfig, outputPolicy)
	if err != nil {
		return err
	}
	if _, err := store.Apply(policies, true, false); err != nil {
		return err
	}
	summary, _ := json.Marshal(map[string]any{
		"keys": len(keys), "policies": len(policies),
		"source_files_unchanged": true,
	})
	fmt.Println(string(summary))
	return nil
}

func replaceAPIKeys(raw []byte, keys []string) ([]byte, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("CPA config root must be a mapping")
	}
	root := document.Content[0]
	var value *yaml.Node
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "api-keys" {
			value = root.Content[index+1]
			break
		}
	}
	if value == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "api-keys"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"},
		)
		value = root.Content[len(root.Content)-1]
	}
	var existing []string
	if value.Kind == yaml.SequenceNode {
		for _, node := range value.Content {
			existing = append(existing, strings.TrimSpace(node.Value))
		}
	}
	value.Kind = yaml.SequenceNode
	value.Tag = "!!seq"
	value.Style = 0
	value.Content = nil
	for _, key := range keys {
		value.Content = append(value.Content, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!str", Value: key, Style: yaml.DoubleQuotedStyle,
		})
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), existing, encoder.Close()
}

func enableNativeAccess(raw []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("CPA config root must be a mapping")
	}
	root := document.Content[0]
	plugins := mappingValue(root, "plugins")
	if plugins == nil || plugins.Kind != yaml.MappingNode {
		return nil, errors.New("CPA config is missing plugins mapping")
	}
	configs := mappingValue(plugins, "configs")
	if configs == nil || configs.Kind != yaml.MappingNode {
		return nil, errors.New("CPA config is missing plugins.configs mapping")
	}
	keyPolicy := mappingValue(configs, "cpa-key-policy")
	if keyPolicy == nil || keyPolicy.Kind != yaml.MappingNode {
		return nil, errors.New("CPA config is missing plugins.configs.cpa-key-policy mapping")
	}

	removeMappingKeys(keyPolicy, "keys", "aliases")
	setMappingScalar(keyPolicy, "mode", "native-access")
	setMappingScalar(keyPolicy, "native_keys_file", "/CLIProxyAPI/config.yaml")
	setMappingScalar(keyPolicy, "native_state_file", "/root/.cli-proxy-api/cpa-key-access-policy-state.json")

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, err
	}
	return out.Bytes(), encoder.Close()
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func setMappingScalar(mapping *yaml.Node, key, value string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func removeMappingKeys(mapping *yaml.Node, keys ...string) {
	remove := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		remove[key] = struct{}{}
	}
	filtered := make([]*yaml.Node, 0, len(mapping.Content))
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if _, found := remove[mapping.Content[index].Value]; found {
			continue
		}
		filtered = append(filtered, mapping.Content[index], mapping.Content[index+1])
	}
	mapping.Content = filtered
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, mode); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func samePath(left, right string) bool {
	a, _ := filepath.Abs(left)
	b, _ := filepath.Abs(right)
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
