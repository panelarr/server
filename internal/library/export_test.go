// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"

	"github.com/google/uuid"

	sourcev1 "github.com/panelarr/source-api/gen/panelarr/source/v1"
)

// SaveMatchesPaused saves series as a source's weak candidates for a work,
// as matching does, and calls pause between dropping the old candidates
// and saving the new ones.
func SaveMatchesPaused(ctx context.Context, s *Service, workID uuid.UUID, sourceID string, pause func(), series ...*sourcev1.Series) error {
	cands := make([]*candidate, 0, len(series))
	for _, se := range series {
		cands = append(cands, &candidate{series: se, score: 0.5})
	}
	_, _, err := s.saveMatchesPaused(ctx, workID, sourceID, cands, nil, pause)
	return err
}
