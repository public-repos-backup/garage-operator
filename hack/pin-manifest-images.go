package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	pinnedImagePattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	imageFlagPattern   = regexp.MustCompile(`(^|[\s'"])--([[:alnum:]_-]+-)?image(=|[[:space:]]|['"]|$)`)
	splitImageFlag     = regexp.MustCompile(`^--([[:alnum:]_-]+-)?image$`)
)

type imagePins struct {
	values map[string]string
	counts map[string]int
}

func parseImagePins(arguments []string) (*imagePins, error) {
	if len(arguments) == 0 {
		return nil, errors.New("at least one image pin mapping is required")
	}
	pins := &imagePins{values: make(map[string]string, len(arguments)), counts: make(map[string]int, len(arguments))}
	for _, argument := range arguments {
		source, pinned, found := strings.Cut(argument, "=")
		if !found || source == "" || strings.ContainsAny(source, "@ \t\r\n") || !pinnedImagePattern.MatchString(pinned) {
			return nil, fmt.Errorf("invalid image pin mapping: %s", argument)
		}
		if _, duplicate := pins.values[source]; duplicate {
			return nil, fmt.Errorf("duplicate image pin mapping: %s", source)
		}
		pins.values[source] = pinned
	}
	return pins, nil
}

func (p *imagePins) pin(reference string) (string, error) {
	if pinnedImagePattern.MatchString(reference) {
		return reference, nil
	}
	if pinned, found := p.values[reference]; found {
		p.counts[reference]++
		return pinned, nil
	}
	return "", fmt.Errorf("manifest contains image reference without a digest: %s", reference)
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	seen := map[*yaml.Node]bool{}
	for node != nil && node.Kind == yaml.AliasNode {
		if node.Alias == nil || seen[node] {
			return nil
		}
		seen[node] = true
		node = node.Alias
	}
	return node
}

func pinImageScalar(node *yaml.Node, pins *imagePins) error {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return errors.New("image field must contain one scalar image reference")
	}
	pinned, err := pins.pin(node.Value)
	if err != nil {
		return err
	}
	node.Value = pinned
	return nil
}

func pinImageFlags(value string, pins *imagePins) (string, error) {
	matches := imageFlagPattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil
	}
	var output strings.Builder
	last := 0
	for _, match := range matches {
		delimiterStart, delimiterEnd := match[6], match[7]
		if delimiterStart == delimiterEnd || value[delimiterStart] == '\'' || value[delimiterStart] == '"' {
			return "", fmt.Errorf("image flag in argument must use an unquoted image reference: %s", value[match[0]:match[1]])
		}
		valueStart := delimiterEnd
		if value[delimiterStart] != '=' {
			for valueStart < len(value) && isArgumentSpace(value[valueStart]) {
				valueStart++
			}
		}
		if valueStart >= len(value) || value[valueStart] == '\'' || value[valueStart] == '"' {
			return "", fmt.Errorf("image flag in argument must be followed by an unquoted image reference: %s", value[match[0]:match[1]])
		}
		valueEnd := valueStart
		for valueEnd < len(value) && !isArgumentSpace(value[valueEnd]) && value[valueEnd] != '\'' && value[valueEnd] != '"' {
			valueEnd++
		}
		pinned, err := pins.pin(value[valueStart:valueEnd])
		if err != nil {
			return "", err
		}
		output.WriteString(value[last:valueStart])
		output.WriteString(pinned)
		last = valueEnd
	}
	output.WriteString(value[last:])
	return output.String(), nil
}

func isArgumentSpace(character byte) bool {
	switch character {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	default:
		return false
	}
}

func pinArgumentSequence(node *yaml.Node, pins *imagePins) error {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.SequenceNode {
		return errors.New("args/command image validation requires a YAML sequence")
	}
	for index := 0; index < len(node.Content); index++ {
		argument := resolveAlias(node.Content[index])
		if argument == nil || argument.Kind != yaml.ScalarNode {
			return errors.New("args/command entries must be scalar strings")
		}
		if splitImageFlag.MatchString(argument.Value) {
			if index+1 >= len(node.Content) {
				return fmt.Errorf("split image flag %q has no following image reference", argument.Value)
			}
			next := resolveAlias(node.Content[index+1])
			if next == nil || next.Kind != yaml.ScalarNode {
				return fmt.Errorf("split image flag %q must be followed by a scalar image reference", argument.Value)
			}
			pinned, err := pins.pin(next.Value)
			if err != nil {
				return err
			}
			next.Value = pinned
			index++
			continue
		}
		pinned, err := pinImageFlags(argument.Value, pins)
		if err != nil {
			return err
		}
		argument.Value = pinned
	}
	return nil
}

func pinYAMLNode(node *yaml.Node, pins *imagePins, visiting map[*yaml.Node]bool) error {
	node = resolveAlias(node)
	if node == nil || visiting[node] {
		return nil
	}
	visiting[node] = true
	defer delete(visiting, node)

	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := pinYAMLNode(child, pins, visiting); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := resolveAlias(node.Content[index]), node.Content[index+1]
			if key != nil && key.Kind == yaml.ScalarNode {
				switch key.Value {
				case "image":
					if err := pinImageScalar(value, pins); err != nil {
						return err
					}
				case "args", "command":
					if err := pinArgumentSequence(value, pins); err != nil {
						return err
					}
				}
			}
			if err := pinYAMLNode(value, pins, visiting); err != nil {
				return err
			}
		}
	}
	return nil
}

func pinManifest(input io.Reader, output io.Writer, arguments []string) error {
	pins, err := parseImagePins(arguments)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(input)
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)

	documents := 0
	for {
		document := &yaml.Node{}
		if err := decoder.Decode(document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode manifest YAML: %w", err)
		}
		if len(document.Content) == 0 {
			continue
		}
		if err := pinYAMLNode(document, pins, make(map[*yaml.Node]bool)); err != nil {
			return err
		}
		if err := encoder.Encode(document); err != nil {
			return fmt.Errorf("encode pinned manifest YAML: %w", err)
		}
		documents++
	}
	if documents == 0 {
		return errors.New("manifest contains no YAML documents")
	}
	for source := range pins.values {
		if pins.counts[source] == 0 {
			return fmt.Errorf("expected image is absent from manifest image fields or arguments: %s", source)
		}
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("finish pinned manifest YAML: %w", err)
	}
	if _, err := io.Copy(output, &buffer); err != nil {
		return fmt.Errorf("write pinned manifest YAML: %w", err)
	}
	return nil
}

func main() {
	if err := pinManifest(os.Stdin, os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
