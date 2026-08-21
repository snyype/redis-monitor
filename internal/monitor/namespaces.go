package monitor

import (
	"context"
	"sort"
	"time"
)

// Namespaces is the "Namespace" dropdown's data: the key prefixes that actually
// exist in one database, largest first.
type Namespaces struct {
	Rows       []NamespaceRow `json:"rows"`
	Scanned    int64          `json:"scanned"`
	DBKeys     int64          `json:"db_keys"`
	Truncated  bool           `json:"truncated"`
	Distinct   int            `json:"distinct"`
	Unprefixed int64          `json:"unprefixed"`
	Capped     bool           `json:"capped"`
	Database   int            `json:"database"`
}

// NamespaceRow carries a ready-made, glob-escaped MATCH pattern so the page never
// has to build one — a prefix containing '*' or '[' still selects only itself.
type NamespaceRow struct {
	Namespace string `json:"namespace"`
	Pattern   string `json:"pattern"`
	Count     int64  `json:"count"`
}

// Namespaces lists the distinct namespaces in a database, cached for
// namespace_cache_seconds.
//
// Much cheaper than the chart sample: it reads key NAMES only, one round trip per
// SCAN batch instead of per key, which is why it can afford a far bigger sample.
// Still bounded, and still honest about it — on a keyspace larger than the sample
// the list is the namespaces that were seen, not a guaranteed-complete set.
func (s *Service) Namespaces(ctx context.Context, db int, fresh bool) (*Namespaces, error) {
	key := cacheKey("namespaces", db)
	ttl := time.Duration(s.cfg.NamespaceCacheSeconds) * time.Second

	if !fresh && ttl > 0 {
		if cached, ok := s.cache.Get(key); ok {
			if namespaces, ok := cached.(*Namespaces); ok {
				return namespaces, nil
			}
		}
	}

	namespaces, err := s.buildNamespaces(ctx, db)
	if err != nil {
		return nil, err
	}

	s.cache.Put(key, namespaces, ttl)

	return namespaces, nil
}

func (s *Service) buildNamespaces(ctx context.Context, db int) (*Namespaces, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	keys := dbSize(ctx, client)
	limit := int64(s.cfg.NamespaceSampleSize)

	result := &Namespaces{
		Rows:     []NamespaceRow{},
		DBKeys:   keys,
		Database: db,
	}

	if limit <= 0 {
		result.Truncated = keys > 0

		return result, nil
	}

	counts := make(map[string]int64)

	var scanned, unprefixed int64

	err = scanBatches(ctx, client, "", int64(s.cfg.ScanCount), s.cfg.MaxScanIterations, func(batch []string) (bool, error) {
		for _, key := range batch {
			if scanned >= limit {
				break
			}

			scanned++

			// A key with no separator has no namespace at all. It is counted, but it
			// gets no dropdown entry: there is no glob that would select exactly the
			// unnamespaced keys.
			namespace, ok := namespaceOf(key)
			if !ok {
				unprefixed++

				continue
			}

			counts[namespace]++
		}

		return scanned < limit, nil
	})
	if err != nil {
		return nil, err
	}

	ranked := make([]NamespaceRow, 0, len(counts))

	for namespace, count := range counts {
		ranked = append(ranked, NamespaceRow{
			Namespace: namespace,
			Pattern:   namespacePattern(namespace),
			Count:     count,
		})
	}

	sort.Slice(ranked, func(a, b int) bool {
		if ranked[a].Count != ranked[b].Count {
			return ranked[a].Count > ranked[b].Count
		}

		return ranked[a].Namespace < ranked[b].Namespace
	})

	distinct := len(ranked)

	if len(ranked) > s.cfg.MaxNamespaces {
		ranked = ranked[:s.cfg.MaxNamespaces]
	}

	result.Rows = ranked
	result.Scanned = scanned
	result.Truncated = keys > scanned
	result.Distinct = distinct
	result.Unprefixed = unprefixed
	result.Capped = distinct > len(ranked)

	return result, nil
}
