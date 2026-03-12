package qos

import "testing"

func TestParsePriority(t *testing.T) {
	p, err := ParsePriority("critical")
	if err != nil {
		t.Fatalf("ParsePriority: %v", err)
	}
	if p != PriorityCritical {
		t.Fatalf("priority = %v, want %v", p, PriorityCritical)
	}
	if _, err := ParsePriority("invalid"); err == nil {
		t.Fatal("expected error for invalid priority")
	}
}

func TestWorkerMultiplierOrder(t *testing.T) {
	if PriorityCritical.WorkerMultiplier() <= PriorityHigh.WorkerMultiplier() {
		t.Fatal("critical must have higher multiplier than high")
	}
	if PriorityBulk.WorkerMultiplier() >= PriorityNormal.WorkerMultiplier() {
		t.Fatal("bulk must have lower multiplier than normal")
	}
}
