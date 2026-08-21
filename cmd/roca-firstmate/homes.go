package main

import (
	"errors"
	"fmt"
	"strings"
)

var (
	errUnbalancedHomes = errors.New("--home and --home-id must be paired positionally")
	errHomeRequired    = errors.New("--home and --home-id are required")
	errHomeFreshness   = errors.New("--home and --home-id are required for ingest-on-read freshness")
	errEmptyHomeID     = errors.New("--home-id must not be blank")
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type homeBinding struct {
	homes   stringList
	homeIDs stringList
	labels  stringList
	kinds   stringList
}

type homePair struct {
	Path  string
	ID    string
	Label string
	Kind  string
}

type homeRequest struct {
	Pairs   []homePair
	Filters []string
}

func pairHomes(homes, ids, labels, kinds []string) ([]homePair, error) {
	if len(homes) != len(ids) {
		return nil, fmt.Errorf("%w (got %d homes and %d home-ids)", errUnbalancedHomes, len(homes), len(ids))
	}
	alignedLabels, err := alignOptional("label", labels, len(homes))
	if err != nil {
		return nil, err
	}
	alignedKinds, err := alignOptional("kind", kinds, len(homes))
	if err != nil {
		return nil, err
	}
	pairs := make([]homePair, len(homes))
	for i := range homes {
		id := strings.TrimSpace(ids[i])
		label := strings.TrimSpace(alignedLabels[i])
		if label == "" {
			label = id
		}
		kind := strings.TrimSpace(alignedKinds[i])
		if kind == "" {
			kind = "primary"
		}
		pairs[i] = homePair{
			Path:  strings.TrimSpace(homes[i]),
			ID:    id,
			Label: label,
			Kind:  kind,
		}
	}
	return pairs, nil
}

func alignOptional(name string, values []string, n int) ([]string, error) {
	if n == 0 {
		if len(values) != 0 {
			return nil, fmt.Errorf("--%s count %d does not match --home count 0", name, len(values))
		}
		return nil, nil
	}
	if len(values) == 0 {
		return make([]string, n), nil
	}
	if len(values) == 1 && n > 1 {
		out := make([]string, n)
		for i := range out {
			out[i] = values[0]
		}
		return out, nil
	}
	if len(values) != n {
		return nil, fmt.Errorf("--%s count %d does not match --home count %d", name, len(values), n)
	}
	return values, nil
}

func resolveHomes(binding homeBinding, envHome, envID string, allowFilterOnly bool) (homeRequest, error) {
	homes := append([]string(nil), binding.homes...)
	ids := append([]string(nil), binding.homeIDs...)
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return homeRequest{}, errEmptyHomeID
		}
	}
	if len(homes) == 0 && len(ids) == 0 {
		if home := strings.TrimSpace(envHome); home != "" {
			homes = []string{home}
		}
		if id := strings.TrimSpace(envID); id != "" {
			ids = []string{id}
		}
	}
	if len(homes) == 0 {
		if !allowFilterOnly {
			return homeRequest{}, errHomeRequired
		}
		filters := make([]string, 0, len(ids))
		for _, id := range ids {
			filters = append(filters, strings.TrimSpace(id))
		}
		return homeRequest{Filters: filters}, nil
	}
	pairs, err := pairHomes(homes, ids, binding.labels, binding.kinds)
	if err != nil {
		return homeRequest{}, err
	}
	return homeRequest{Pairs: pairs}, nil
}
