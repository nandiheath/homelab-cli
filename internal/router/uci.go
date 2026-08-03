package router

import (
	"bufio"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

func parseUCIExport(packageName, text string) (UCIPackage, error) {
	pkg := UCIPackage{Package: packageName}
	var current *UCISection
	scanner := bufio.NewScanner(strings.NewReader(text))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields, err := splitUCIFields(line)
		if err != nil {
			return UCIPackage{}, fmt.Errorf("%s line %d: %w", packageName, lineNumber, err)
		}
		switch fields[0] {
		case "package":
			if len(fields) != 2 || fields[1] != packageName {
				return UCIPackage{}, fmt.Errorf("%s line %d: invalid package declaration", packageName, lineNumber)
			}
		case "config":
			if len(fields) < 2 || len(fields) > 3 {
				return UCIPackage{}, fmt.Errorf("%s line %d: invalid config declaration", packageName, lineNumber)
			}
			section := UCISection{Type: fields[1], Options: make(map[string]UCIValue), Lists: make(map[string][]UCIValue)}
			if len(fields) == 3 {
				section.Name = fields[2]
			}
			pkg.Sections = append(pkg.Sections, section)
			current = &pkg.Sections[len(pkg.Sections)-1]
		case "option":
			if current == nil || len(fields) != 3 {
				return UCIPackage{}, fmt.Errorf("%s line %d: invalid option", packageName, lineNumber)
			}
			value := fields[2]
			current.Options[fields[1]] = UCIValue{Literal: &value}
		case "list":
			if current == nil || len(fields) != 3 {
				return UCIPackage{}, fmt.Errorf("%s line %d: invalid list", packageName, lineNumber)
			}
			value := fields[2]
			current.Lists[fields[1]] = append(current.Lists[fields[1]], UCIValue{Literal: &value})
		default:
			return UCIPackage{}, fmt.Errorf("%s line %d: unsupported directive %q", packageName, lineNumber, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return UCIPackage{}, err
	}
	return pkg, nil
}

func splitUCIFields(line string) ([]string, error) {
	var fields []string
	var token strings.Builder
	var quote rune
	escaped := false
	inToken := false
	flush := func() {
		if inToken {
			fields = append(fields, token.String())
			token.Reset()
			inToken = false
		}
	}
	for _, character := range line {
		if escaped {
			token.WriteRune(character)
			escaped = false
			inToken = true
			continue
		}
		if character == '\\' {
			escaped = true
			inToken = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				token.WriteRune(character)
			}
			inToken = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			inToken = true
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		token.WriteRune(character)
		inToken = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape")
	}
	flush()
	if len(fields) == 0 {
		return nil, errors.New("empty directive")
	}
	return fields, nil
}

type planResult struct {
	Lines          []string
	Drift          int
	SkippedSecrets int
}

func comparePackage(desired, actual UCIPackage, secretsResolved bool) planResult {
	var result planResult
	if len(desired.Sections) != len(actual.Sections) {
		result.Lines = append(result.Lines, fmt.Sprintf("~ %s sections: %d -> %d", desired.Package, len(actual.Sections), len(desired.Sections)))
		result.Drift++
	}
	maximum := len(desired.Sections)
	if len(actual.Sections) > maximum {
		maximum = len(actual.Sections)
	}
	for index := range maximum {
		if index >= len(desired.Sections) {
			result.Lines = append(result.Lines, fmt.Sprintf("- %s section %d (%s)", desired.Package, index+1, sectionIdentity(actual.Sections[index])))
			result.Drift++
			continue
		}
		if index >= len(actual.Sections) {
			result.Lines = append(result.Lines, fmt.Sprintf("+ %s section %d (%s)", desired.Package, index+1, sectionIdentity(desired.Sections[index])))
			result.Drift++
			continue
		}
		desiredSection := desired.Sections[index]
		actualSection := actual.Sections[index]
		location := fmt.Sprintf("%s.%s", desired.Package, sectionIdentity(desiredSection))
		if desiredSection.Type != actualSection.Type || desiredSection.Name != actualSection.Name {
			result.Lines = append(result.Lines, fmt.Sprintf("~ %s identity: %s/%s -> %s/%s", desired.Package, actualSection.Type, sectionIdentity(actualSection), desiredSection.Type, sectionIdentity(desiredSection)))
			result.Drift++
			continue
		}
		compareOptions(&result, location, desired.Package, desiredSection.Options, actualSection.Options, secretsResolved)
		compareLists(&result, location, desired.Package, desiredSection.Lists, actualSection.Lists, secretsResolved)
	}
	return result
}

func compareOptions(result *planResult, location, packageName string, desired, actual map[string]UCIValue, secretsResolved bool) {
	keys := unionKeys(desired, actual)
	for _, key := range keys {
		desiredValue, desiredFound := desired[key]
		actualValue, actualFound := actual[key]
		path := location + "." + key
		if !desiredFound {
			result.Lines = append(result.Lines, "- "+path)
			result.Drift++
			continue
		}
		if !actualFound {
			if desiredValue.SecretRef != "" {
				result.Lines = append(result.Lines, "+ "+path+" = <managed secret>")
			} else {
				result.Lines = append(result.Lines, "+ "+path+" = "+quotePlan(*desiredValue.Literal))
			}
			result.Drift++
			continue
		}
		if desiredValue.SecretRef != "" && !secretsResolved {
			result.Lines = append(result.Lines, "? "+path+" = <managed secret; comparison skipped>")
			result.SkippedSecrets++
			continue
		}
		if valueString(desiredValue) != valueString(actualValue) {
			if desiredValue.SecretRef != "" || isSensitiveOption(packageName, key, valueString(actualValue)) {
				result.Lines = append(result.Lines, "~ "+path+" = <managed secret differs>")
			} else {
				result.Lines = append(result.Lines, "~ "+path+": "+quotePlan(valueString(actualValue))+" -> "+quotePlan(valueString(desiredValue)))
			}
			result.Drift++
		}
	}
}

func compareLists(result *planResult, location, packageName string, desired, actual map[string][]UCIValue, secretsResolved bool) {
	keys := unionKeys(desired, actual)
	for _, key := range keys {
		desiredValues, desiredFound := desired[key]
		actualValues, actualFound := actual[key]
		path := location + "." + key
		if !desiredFound || !actualFound {
			result.Lines = append(result.Lines, "~ "+path+" list membership")
			result.Drift++
			continue
		}
		if containsSecret(desiredValues) && !secretsResolved {
			result.Lines = append(result.Lines, "? "+path+" = <managed secret list; comparison skipped>")
			result.SkippedSecrets++
			continue
		}
		if !equalValues(desiredValues, actualValues) {
			if containsSecret(desiredValues) || isSensitiveOption(packageName, key, "") {
				result.Lines = append(result.Lines, "~ "+path+" = <managed secret list differs>")
			} else {
				result.Lines = append(result.Lines, "~ "+path+" list membership")
			}
			result.Drift++
		}
	}
}

func unionKeys[V any](left, right map[string]V) []string {
	set := make(map[string]bool, len(left)+len(right))
	for key := range left {
		set[key] = true
	}
	for key := range right {
		set[key] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func valueString(value UCIValue) string {
	if value.Literal == nil {
		return ""
	}
	return *value.Literal
}

func containsSecret(values []UCIValue) bool {
	for _, value := range values {
		if value.SecretRef != "" {
			return true
		}
	}
	return false
}

func equalValues(left, right []UCIValue) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if valueString(left[index]) != valueString(right[index]) {
			return false
		}
	}
	return true
}

func quotePlan(value string) string {
	if len(value) > 80 {
		value = value[:77] + "..."
	}
	return fmt.Sprintf("%q", value)
}
