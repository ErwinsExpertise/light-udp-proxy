package qos

import (
	"fmt"
	"strings"
)

// Priority defines frontend processing priority.
type Priority uint8

const (
	PriorityBulk Priority = iota
	PriorityLow
	PriorityNormal
	PriorityHigh
	PriorityCritical
)

// ParsePriority parses a priority string.
func ParsePriority(v string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "critical":
		return PriorityCritical, nil
	case "high":
		return PriorityHigh, nil
	case "normal", "":
		return PriorityNormal, nil
	case "low":
		return PriorityLow, nil
	case "bulk":
		return PriorityBulk, nil
	default:
		return PriorityNormal, fmt.Errorf("unknown priority %q", v)
	}
}

// String returns the YAML string value for the priority.
func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "critical"
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	case PriorityBulk:
		return "bulk"
	default:
		return "normal"
	}
}

// WorkerMultiplier returns the relative worker scaling factor.
func (p Priority) WorkerMultiplier() int {
	switch p {
	case PriorityCritical:
		return 5
	case PriorityHigh:
		return 4
	case PriorityLow:
		return 2
	case PriorityBulk:
		return 1
	default:
		return 3
	}
}
