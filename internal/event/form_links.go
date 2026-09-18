package event

import (
	"bytes"
	"encoding/json"
)

type EventFormLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Alias string `json:"alias,omitempty"`
}

type extraFormDoc struct {
	ApplyAlias string          `json:"applyAlias,omitempty"`
	Extra      []EventFormLink `json:"extra"`
}

func EncodeExtraForm(alias string, extra []EventFormLink) ([]byte, error) {
	if extra == nil {
		extra = []EventFormLink{}
	}
	return json.Marshal(extraFormDoc{ApplyAlias: alias, Extra: extra})
}

func DecodeExtraForm(raw []byte) (string, []EventFormLink, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", []EventFormLink{}, nil
	}
	if trimmed[0] == '[' {
		var extra []EventFormLink
		if err := json.Unmarshal(trimmed, &extra); err != nil {
			return "", nil, err
		}
		if extra == nil {
			extra = []EventFormLink{}
		}
		return "", extra, nil
	}
	var doc extraFormDoc
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return "", nil, err
	}
	if doc.Extra == nil {
		doc.Extra = []EventFormLink{}
	}
	return doc.ApplyAlias, doc.Extra, nil
}
