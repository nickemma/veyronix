package backend

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickemma/plinth/internal/manifest"
)

func TestFakeZeroReplicasStillCreatesGoldenPath(t *testing.T) {
	m := manifest.Manifest{
		Name: "paused-service", Image: "ghcr.io/example/paused:v1", Port: 8080, Replicas: 0,
		Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"},
	}
	result, err := NewFake().Ensure(context.Background(), m, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || len(result.Resources) != 9 {
		t.Fatalf("expected ready zero-replica golden path, got %+v", result)
	}
}

func TestProgressiveRolloutUsesCeilingReplicaCounts(t *testing.T) {
	if got := rolloutReplicas(3, 10); got != 1 {
		t.Fatalf("expected 10%% of three replicas to stage one replica, got %d", got)
	}
	if got := rolloutReplicas(3, 50); got != 2 {
		t.Fatalf("expected 50%% of three replicas to stage two replicas, got %d", got)
	}
}

type measuredFake struct {
	*Fake
	rates []float64
	calls int
	seen  []int
}

func (b *measuredFake) Ensure(ctx context.Context, m manifest.Manifest, revision int) (ApplyResult, error) {
	b.seen = append(b.seen, m.Replicas)
	return b.Fake.Ensure(ctx, m, revision)
}

func (b *measuredFake) ErrorRate(context.Context, manifest.Manifest) (float64, error) {
	if b.calls >= len(b.rates) {
		return 0, nil
	}
	rate := b.rates[b.calls]
	b.calls++
	return rate, nil
}

func TestProgressiveRolloutAbortsFromExternalErrorRate(t *testing.T) {
	target := &measuredFake{Fake: NewFake(), rates: []float64{0, 0.2}}
	m := manifest.Manifest{
		Name: "measured-service", Image: "ghcr.io/example/measured:v1", Port: 8080, Replicas: 10,
		Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"},
		Rollout:   manifest.Rollout{Enabled: true, Steps: []int{10, 100}, MaxErrorRate: 0.05},
	}
	result, err := EnsureWithRollout(context.Background(), target, m, 1)
	if err == nil || target.calls != 2 {
		t.Fatalf("expected external error-rate abort after two stages, result=%+v err=%v calls=%d", result, err, target.calls)
	}
	if result.ErrorRate != 0.2 {
		t.Fatalf("expected measured error rate in result, got %v", result.ErrorRate)
	}
}

func TestProgressiveRolloutResumesAfterCompletedStage(t *testing.T) {
	target := &measuredFake{Fake: NewFake()}
	m := manifest.Manifest{
		Name: "resumed-service", Image: "ghcr.io/example/resumed:v1", Port: 8080, Replicas: 10,
		Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"},
		Rollout:   manifest.Rollout{Enabled: true, Steps: []int{10, 50, 100}, MaxErrorRate: 0.05},
	}
	result, err := EnsureWithRolloutFrom(context.Background(), target, m, 1, 10)
	if err != nil || !result.Ready {
		t.Fatalf("expected completed rollout to re-ensure at full scale, result=%+v err=%v", result, err)
	}
	if got, want := fmt.Sprint(target.seen), "[5 10]"; got != want {
		t.Fatalf("expected resumed rollout to skip earlier stages, got %s", got)
	}
	if result.RolloutStep != 100 {
		t.Fatalf("expected resumed rollout to finish at step 100, got %d", result.RolloutStep)
	}
}
