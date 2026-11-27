package agents

import (
	"context"
	"time"
)

// Removing a personal agent conversation (#6433 follow-up). The conversation belongs to its source (the Lark gateway
// through its extension), which has no way to delete or archive one, and the list is pulled from it again every
// minute. So "remove" is Persona's own: the session id goes into Prefs.Hidden with when, and the list, its pages and
// counts and unread totals leave it out. A message after that time (the source's rankTimeMs moving past it) brings it
// back by itself, as a deleted agent session comes back when used again; Unhide brings it back by hand.

// maxHidden bounds Prefs.Hidden; past it the oldest entries go (the source has long stopped listing most of them).
const maxHidden = 5000

// hiddenStill reports whether s stays out of the list: hidden, and nothing newer than the hide arrived since.
func hiddenStill(s Session, hidden map[string]int64) bool {
	at, ok := hidden[s.ID]
	return ok && s.RankTimeMs <= at
}

// hiddenNow is the stored hidden set, or nil when none (or when the prefs cannot be read: a list is better shown
// whole than not at all).
func (s *Service) hiddenNow(ctx context.Context) map[string]int64 {
	p, err := s.prefs.get(ctx)
	if err != nil {
		s.logf("agents: hidden: %v", err)
		return nil
	}
	return p.Hidden
}

// listPrefsNow is what the paged index reads of the prefs: the hidden sessions and each pinned one's place (1 first).
func (s *Service) listPrefsNow(ctx context.Context) (map[string]int64, map[string]int) {
	p, err := s.prefs.get(ctx)
	if err != nil {
		s.logf("agents: prefs: %v", err)
		return nil, nil
	}
	pins := make(map[string]int, len(p.Pins))
	for i, id := range p.Pins {
		pins[id] = i + 1
	}
	return p.Hidden, pins
}

// withoutHidden is list less what the user removed.
func withoutHidden(list []Session, hidden map[string]int64) []Session {
	if len(hidden) == 0 {
		return list
	}
	out := make([]Session, 0, len(list))
	for _, x := range list {
		if !hiddenStill(x, hidden) {
			out = append(out, x)
		}
	}
	return out
}

// Hide takes session id out of Persona's list until it gets a new message. The session must be one a source lists
// now, so the hide time is the last message the user saw go.
func (s *Service) Hide(ctx context.Context, id string) (Prefs, error) {
	x, err := s.Session(ctx, id)
	if err != nil {
		return Prefs{}, err
	}
	at := max(x.RankTimeMs, time.Now().UnixMilli())
	p, err := s.prefs.update(ctx, func(p *Prefs) error {
		if p.Hidden == nil {
			p.Hidden = map[string]int64{}
		}
		p.Hidden[id] = at
		trimHidden(p.Hidden)
		return nil
	})
	if err == nil {
		s.reindex()
	}
	return p, err
}

// Unhide puts session id back in the list.
func (s *Service) Unhide(ctx context.Context, id string) (Prefs, error) {
	if _, _, err := s.source(id); err != nil {
		return Prefs{}, err
	}
	p, err := s.prefs.update(ctx, func(p *Prefs) error {
		delete(p.Hidden, id)
		return nil
	})
	if err == nil {
		s.reindex()
	}
	return p, err
}

// Hidden lists the removed conversations the sources still list, newest first, each marked so the apps can offer to
// bring it back. One that got a message since is not here: it is back in the list already.
func (s *Service) Hidden(ctx context.Context) ([]Session, error) {
	hidden := s.hiddenNow(ctx)
	out := []Session{}
	if len(hidden) == 0 {
		return out, nil
	}
	all, err := s.allSessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, x := range all {
		if hiddenStill(x, hidden) {
			out = append(out, x)
		}
	}
	return out, nil
}

// trimHidden drops the oldest entries past maxHidden.
func trimHidden(h map[string]int64) {
	for len(h) > maxHidden {
		oldest, at := "", int64(0)
		for k, t := range h {
			if oldest == "" || t < at {
				oldest, at = k, t
			}
		}
		delete(h, oldest)
	}
}

// reindex rebuilds the paged index from the cached lists so a hide or a pin shows at once, and tells the apps to re-pull.
func (s *Service) reindex() {
	for _, src := range s.all() {
		if list, ok := s.cache.getSessions(src.Name()); ok {
			s.indexSessions(src.Name(), list)
		}
	}
	s.onChange([]string{})
}
