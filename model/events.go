package model

import "time"

// EventBatch is the payload SDKs post to record experiment data.
type EventBatch struct {
	Exposures   []ExposureEvent   `json:"exposures,omitempty"`
	Conversions []ConversionEvent `json:"conversions,omitempty"`
}

// ExposureEvent records that a context was served a variant of a flag
// that is running an experiment.
type ExposureEvent struct {
	Flag       string    `json:"flag"`
	Variant    string    `json:"variant"`
	ContextKey string    `json:"contextKey"`
	Timestamp  time.Time `json:"timestamp"`
}

// ConversionEvent records that a context performed a tracked action.
type ConversionEvent struct {
	Event      string    `json:"event"`
	ContextKey string    `json:"contextKey"`
	Timestamp  time.Time `json:"timestamp"`
}

// MaxEventBatch is the largest number of exposures plus conversions
// accepted in one batch.
const MaxEventBatch = 5000
