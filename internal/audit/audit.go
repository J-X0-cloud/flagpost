// Package audit defines audit log entries and computes the structural diff
// recorded with every change, so the log answers "what exactly changed"
// rather than only "something changed".
package audit

import (
	"encoding/json"
	"time"
)

// Actions recorded in the audit log.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionDelete    = "delete"
	ActionToggle    = "toggle"
	ActionRotateKey = "rotate_key"
)

// Resource types recorded in the audit log.
const (
	ResourceEnvironment = "environment"
	ResourceFlag        = "flag"
	ResourceFlagConfig  = "flag_config"
	ResourceSegment     = "segment"
)

// Entry is one row of the audit log.
type Entry struct {
	ID           int64           `json:"id"`
	At           time.Time       `json:"at"`
	Actor        string          `json:"actor"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resourceType"`
	ResourceKey  string          `json:"resourceKey"`
	Environment  string          `json:"environment,omitempty"`
	Comment      string          `json:"comment,omitempty"`
	Before       json.RawMessage `json:"before,omitempty"`
	After        json.RawMessage `json:"after,omitempty"`
	Changes      []Change        `json:"changes,omitempty"`
}

// Filter narrows an audit log query. Zero fields do not filter.
type Filter struct {
	Environment  string
	ResourceType string
	ResourceKey  string
	Actor        string
	// BeforeID returns entries with an ID lower than this, for paging
	// backwards through history.
	BeforeID int64
	Limit    int
}
