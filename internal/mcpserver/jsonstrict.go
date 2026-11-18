package mcpserver

import (
	"bytes"
	"encoding/json"
)

// maxJSONDepth bounds the nesting the duplicate check walks; deeper bodies count as having duplicates.
const maxJSONDepth = 64

// hasDuplicateNames reports whether any JSON object in body, at any depth, repeats a member name. Malformed JSON is
// reported as false; the caller's decoder rejects it.
func hasDuplicateNames(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	duplicate, _ := scanValue(decoder, 0)

	return duplicate
}

// scanValue walks one JSON value; it returns whether a duplicate was found and whether the walk stayed well-formed.
func scanValue(decoder *json.Decoder, depth int) (bool, bool) {
	if depth > maxJSONDepth {
		return true, false
	}

	token, err := decoder.Token()
	if err != nil {
		return false, false
	}

	switch token {
	case json.Delim('{'):
		return scanObject(decoder, depth)
	case json.Delim('['):
		return scanArray(decoder, depth)
	default:
		return false, true
	}
}

// scanObject walks the members of one object after its opening delimiter.
func scanObject(decoder *json.Decoder, depth int) (bool, bool) {
	seen := make(map[string]struct{})

	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return false, false
		}

		name, ok := nameToken.(string)
		if !ok {
			return false, false
		}

		if _, exists := seen[name]; exists {
			return true, true
		}

		seen[name] = struct{}{}

		if duplicate, wellFormed := scanValue(decoder, depth+1); duplicate || !wellFormed {
			return duplicate, wellFormed
		}
	}

	_, err := decoder.Token()

	return false, err == nil
}

// scanArray walks the elements of one array after its opening delimiter.
func scanArray(decoder *json.Decoder, depth int) (bool, bool) {
	for decoder.More() {
		if duplicate, wellFormed := scanValue(decoder, depth+1); duplicate || !wellFormed {
			return duplicate, wellFormed
		}
	}

	_, err := decoder.Token()

	return false, err == nil
}
