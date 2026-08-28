package domain

import "time"

type Establishment struct {
	ID          string
	Name        string
	Kind        string
	Description string
}

type Menu struct {
	Establishment Establishment
	Items         []MenuItem
}

type MenuSyncItem struct {
	ExternalID  string
	Name        string
	Description string
	PriceMinor  int64
	Available   bool
}

type MenuSyncResult struct {
	ActiveItems      int
	DeactivatedItems int
	SyncedAt         time.Time
}
