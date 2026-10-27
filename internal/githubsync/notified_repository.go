package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// syncNotifiedRepository shares the source's writer/GC lock but never performs
// source discovery or reconciles another repository's control/publication state.
func (s *Sync) syncNotifiedRepository(ctx context.Context, fullName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repositorySite(fullName); !ok {
		return errors.New("repository notification is outside the source policy")
	}
	response, err := s.request(ctx, repoEndpoint(fullName), "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	const maxRepositoryMetadata = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRepositoryMetadata+1))
	if err != nil {
		return err
	}
	if len(body) > maxRepositoryMetadata {
		return errors.New("GitHub repository metadata exceeds limit")
	}
	var repo repository
	if err := json.Unmarshal(body, &repo); err != nil {
		return errors.New("GitHub returned invalid repository metadata")
	}
	name, ok := s.repositorySite(repo.FullName)
	_, tail, identity := strings.Cut(repo.FullName, "/")
	if !ok || !identity || repo.ID <= 0 || repo.Name != tail || !strings.EqualFold(repo.FullName, fullName) {
		return errors.New("GitHub returned a different or invalid repository identity")
	}
	return s.syncRepository(ctx, repo, name)
}
