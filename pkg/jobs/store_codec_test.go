package jobs

import (
	"errors"
	"reflect"
	"testing"
)

func TestSelectEvictionVictims(t *testing.T) {
	limits := StoreLimits{
		MaxJobs:        5,
		MaxJobsPerUser: 3,
	}

	t.Run("no eviction needed when under limits", func(t *testing.T) {
		got, err := selectEvictionVictims(4, 2, []string{"c1"}, []string{"o1"}, limits, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no victims, got %v", got)
		}
	})

	t.Run("evicts caller own inactive first then other org inactive when allowed", func(t *testing.T) {
		got, err := selectEvictionVictims(
			6, // exceeds MaxJobs=5 -> need 2 total evictions
			3, // hits MaxJobsPerUser=3 -> need 1 user eviction, then 1 more org eviction
			[]string{"c1", "c2"},
			[]string{"o1", "o2"},
			limits,
			true,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"c1", "c2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("victims = %v, want %v", got, want)
		}
	})

	t.Run("spills over to other org inactive when caller has no remaining inactive", func(t *testing.T) {
		got, err := selectEvictionVictims(
			5, // hits MaxJobs=5 -> need 1 org eviction
			1,
			nil,
			[]string{"o1", "o2"},
			limits,
			true,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"o1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("victims = %v, want %v", got, want)
		}
	})

	t.Run("import mode never evicts another user's jobs", func(t *testing.T) {
		_, err := selectEvictionVictims(
			5, // hits MaxJobs=5 -> need 1 eviction
			1,
			nil, // caller has no inactive jobs
			[]string{"o1", "o2"},
			limits,
			false,
		)
		if !errors.Is(err, ErrConcurrencyLimited) {
			t.Fatalf("err = %v, want ErrConcurrencyLimited", err)
		}
	})

	t.Run("import mode evicts caller inactive jobs to satisfy org cap", func(t *testing.T) {
		got, err := selectEvictionVictims(
			5,
			2,
			[]string{"c1", "c2"},
			[]string{"o1"},
			limits,
			false,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"c1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("victims = %v, want %v", got, want)
		}
	})
}

func TestParseCancelNotification(t *testing.T) {
	if id, tok, ok := parseCancelNotification("job-abc123:42"); !ok || id != "job-abc123" || tok != 42 {
		t.Fatalf("got (%q, %d, %v), want (job-abc123, 42, true)", id, tok, ok)
	}
	for _, bad := range []string{"", "no-colon", ":12", "job-1:0", "job-1:abc", "../bad:5", "job-1:"} {
		if _, _, ok := parseCancelNotification(bad); ok {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}
