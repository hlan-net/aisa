// Package models knows the models aisa offers: the names in the backends' meta models
// (docs/concepts/CONSUL.md, Backends) and their descriptions in Consul KV, aisa/models/<model>,
// with the other names a model may be asked for under, its aliases (#89).
package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hlan-net/aisa/internal/consul"
)

// MaxName is the longest model name or alias, in bytes: what the decision takes.
const MaxName = 256

// The vocabularies of a model's description: KubeAI's features, and capabilities.
var (
	KnownFeatures     = []string{"TextGeneration", "TextEmbedding", "Reranking", "SpeechToText"}
	KnownCapabilities = []string{"tools", "vision", "json_mode"}
)

// Description is a model's description, aisa/models/<model> in Consul KV.
type Description struct {
	Features      []string `json:"features"`
	ContextLength int64    `json:"contextLength"`
	Capabilities  []string `json:"capabilities"`
	// Aliases are other names the model is asked for under, such as an Ollama tag.
	Aliases []string `json:"aliases"`
}

// Default is the description of a model without one, or with one that cannot be read.
func Default() Description {
	return Description{Features: []string{"TextGeneration"}}
}

// Parse reads a description. It checks the description alone; whether its aliases collide with
// other names is for Index to see.
func Parse(value []byte) (Description, error) {
	var d Description
	// json.Unmarshal takes null as well, as an empty description.
	if trimmed := bytes.TrimSpace(value); len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(value, &d) != nil {
		return Description{}, errors.New(`want a JSON object such as {"features": ["TextGeneration"], "contextLength": 32768, "capabilities": ["tools"], "aliases": ["llama3.2:3b"]}`)
	}
	if len(d.Features) == 0 {
		d.Features = []string{"TextGeneration"}
	}
	for _, f := range d.Features {
		if !slices.Contains(KnownFeatures, f) {
			return Description{}, fmt.Errorf("unknown feature %q, want one of %v", f, KnownFeatures)
		}
	}
	for _, c := range d.Capabilities {
		if !slices.Contains(KnownCapabilities, c) {
			return Description{}, fmt.Errorf("unknown capability %q, want one of %v", c, KnownCapabilities)
		}
	}
	if d.ContextLength < 0 {
		return Description{}, errors.New("contextLength may not be negative")
	}
	for _, a := range d.Aliases {
		if a == "" || strings.TrimSpace(a) != a || len(a) > MaxName {
			return Description{}, fmt.Errorf("invalid alias %q: want a name of 1 to %d bytes without surrounding white space", a, MaxName)
		}
	}
	d.Features = slices.Compact(slices.Sorted(slices.Values(d.Features)))
	d.Capabilities = slices.Compact(slices.Sorted(slices.Values(d.Capabilities)))
	d.Aliases = slices.Compact(slices.Sorted(slices.Values(d.Aliases)))
	return d, nil
}

// Names returns the models of the backends in the catalog, healthy or not, with or without a
// meta the proxy renders: every name the holder put in a backend's models.
func Names(instances []consul.Instance) map[string]bool {
	names := map[string]bool{}
	for _, in := range instances {
		for name := range strings.SplitSeq(in.Meta["models"], ",") {
			if name = strings.TrimSpace(name); name != "" {
				names[name] = true
			}
		}
	}
	return names
}

// Index is what the descriptions say once they are read together with the catalog's models.
type Index struct {
	// Descriptions are the descriptions that could be read, by model.
	Descriptions map[string]Description
	// Invalid are those that could not, with why, and InvalidNames their models in order. A
	// description whose alias is a model's name, or that claims an alias another description
	// claims too, is invalid: an alias names one model or none.
	Invalid      map[string]error
	InvalidNames []string
	// Aliases maps each alias of a valid description to its model.
	Aliases map[string]string
}

// NewIndex reads the descriptions in pairs (keys relative to aisa/models/) against the models
// of the catalog.
func NewIndex(pairs []consul.Pair, catalog map[string]bool) Index {
	descs, invalid, _ := consul.ParseEntries(pairs, Parse)
	// A model's name is one the catalog has, or one with a description.
	isModel := func(name string) bool {
		_, valid := descs[name]
		_, bad := invalid[name]
		return catalog[name] || valid || bad
	}
	maps.Copy(invalid, collisions(descs, isModel))
	aliases := map[string]string{}
	for model, d := range descs {
		if _, bad := invalid[model]; bad {
			delete(descs, model)
			continue
		}
		for _, a := range d.Aliases {
			aliases[a] = model
		}
	}
	return Index{Descriptions: descs, Invalid: invalid, InvalidNames: slices.Sorted(maps.Keys(invalid)), Aliases: aliases}
}

// collisions returns the descriptions made invalid by their aliases: one that is a model's
// name, or one that more than one description claims.
func collisions(descs map[string]Description, isModel func(string) bool) map[string]error {
	claims := map[string][]string{}
	for model, d := range descs {
		for _, a := range d.Aliases {
			claims[a] = append(claims[a], model)
		}
	}
	out := map[string]error{}
	for a, models := range claims {
		var err error
		switch {
		case len(models) > 1:
			sort.Strings(models)
			err = fmt.Errorf("alias %q is claimed by more than one model: %v", a, models)
		case isModel(a):
			err = fmt.Errorf("alias %q is the name of a model", a)
		default:
			continue
		}
		for _, m := range models {
			out[m] = err
		}
	}
	return out
}

// Of returns a model's description: its own when it could be read, the default otherwise.
func (ix Index) Of(model string) Description {
	if d, ok := ix.Descriptions[model]; ok {
		return d
	}
	return Default()
}
