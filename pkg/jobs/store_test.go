package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
	"github.com/yuuki/grafana-llm-studio-app/pkg/llm"
)

type storeFactory struct {
	name string
	new  func(t *testing.T) (jobs.JobStore, func())
}

func allStoreFactories() []storeFactory {
	return []storeFactory{
		{
			name: "memory",
			new: func(_ *testing.T) (jobs.JobStore, func()) {
				s := jobs.NewMemoryStore()
				return s, func() { _ = s.Close() }
			},
		},
		{
			name: "redis",
			new: func(t *testing.T) (jobs.JobStore, func()) {
				t.Helper()
				mr := miniredis.RunT(t)
				s, err := jobs.NewRedisStore(jobs.RedisConfig{
					Addr:      mr.Addr(),
					KeyPrefix: "teststudio:jobs",
				})
				if err != nil {
					t.Fatalf("NewRedisStore: %v", err)
				}
				return s, func() {
					_ = s.Close()
					mr.Close()
				}
			},
		},
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(t0 time.Time) *testClock {
	return &testClock{now: t0.UTC()}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestJobStore_CrossReplicaPersistenceAndRestartSurvival(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			cfg := jobs.Config{
				AllowedDSUIDs: []string{"prom-main", "vm-main"},
				DatasourceDialects: map[string]string{
					"vm-main": "metricsql",
				},
				MaxPanels:            6,
				MaxRepairAttempts:    2,
				MaxRepairAttemptsSet: true,
				Store:                store,
			}

			// Replica 1 generates initial draft (rev 1).
			mgr1 := jobs.NewManager(&llm.MockClient{}, cfg)
			alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}

			s1, err := mgr1.Generate(context.Background(), alice, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate, 5xx error rate, and p95 latency",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("replica 1 Generate failed: %v", err)
			}
			if s1.CurrentIR == nil || s1.CurrentIR.Revision != 1 || s1.CanUndo {
				t.Fatalf("unexpected rev 1 snapshot: %+v", s1)
			}

			// Simulate Replica 1 shutdown/restart — shared store must remain intact.
			mgr1.Close()

			// Replica 2 continues seamlessly from the same job.
			mgr2 := jobs.NewManager(&llm.MockClient{}, cfg)
			defer mgr2.Close()

			got, err := mgr2.Get(alice, s1.JobID)
			if err != nil {
				t.Fatalf("replica 2 Get after replica 1 restart failed: %v", err)
			}
			if got.JobID != s1.JobID || got.CurrentIR == nil || got.CurrentIR.Revision != 1 {
				t.Fatalf("replica 2 Get returned mismatched state: %+v", got)
			}

			// Patch rev 1 -> rev 2 on Replica 2.
			s2, err := mgr2.Patch(context.Background(), alice, s1.JobID, jobs.PatchInput{
				BaseRevision:  1,
				TargetPanelID: "panel-error-rate",
				EditIntent:    "Switch error rate panel to timeseries",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("replica 2 Patch rev 1->2 failed: %v", err)
			}
			if s2.CurrentIR.Revision != 2 || !s2.CanUndo {
				t.Fatalf("expected rev 2 with CanUndo=true, got rev=%d canUndo=%v", s2.CurrentIR.Revision, s2.CanUndo)
			}

			// Patch rev 2 -> rev 3 on Replica 3.
			mgr3 := jobs.NewManager(&llm.MockClient{}, cfg)
			defer mgr3.Close()
			s3, err := mgr3.Patch(context.Background(), alice, s1.JobID, jobs.PatchInput{
				BaseRevision:  2,
				TargetPanelID: "panel-error-rate",
				EditIntent:    "Change title to Error Ratio",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("replica 3 Patch rev 2->3 failed: %v", err)
			}
			if s3.CurrentIR.Revision != 3 || !s3.CanUndo {
				t.Fatalf("expected rev 3 with CanUndo=true, got %+v", s3)
			}

			// Undo on Replica 2: rev 3 -> rev 4 (restoring rev 2).
			u1, err := mgr2.Undo(alice, s1.JobID)
			if err != nil {
				t.Fatalf("replica 2 Undo #1 failed: %v", err)
			}
			if u1.CurrentIR.Revision != 4 || !u1.CanUndo {
				t.Fatalf("expected rev 4 with CanUndo=true, got rev=%d canUndo=%v", u1.CurrentIR.Revision, u1.CanUndo)
			}
			if len(u1.LastDiff) != 1 || u1.LastDiff[0].After != "restored from rev 2 as rev 4" {
				t.Fatalf("unexpected undo diff: %+v", u1.LastDiff)
			}

			// Undo on Replica 3: rev 4 -> rev 5 (restoring rev 1).
			u2, err := mgr3.Undo(alice, s1.JobID)
			if err != nil {
				t.Fatalf("replica 3 Undo #2 failed: %v", err)
			}
			if u2.CurrentIR.Revision != 5 || u2.CanUndo {
				t.Fatalf("expected rev 5 with CanUndo=false, got rev=%d canUndo=%v", u2.CurrentIR.Revision, u2.CanUndo)
			}

			// Third Undo must fail with ErrNoUndoHistory.
			if _, err := mgr2.Undo(alice, s1.JobID); !errors.Is(err, jobs.ErrNoUndoHistory) {
				t.Fatalf("expected ErrNoUndoHistory, got %v", err)
			}

			// Cross-replica knownGauges enforcement: Import gauge on Replica 2 -> spoof counter on Replica 3.
			gaugeDraft := ir.DashboardIR{
				SchemaVersion: ir.SchemaVersion,
				Revision:      1,
				Title:         "Memory",
				TimeRange:     ir.TimeRange{From: "now-15m", To: "now"},
				Panels: []ir.PanelIR{
					{
						ID:            "panel-mem",
						Type:          "stat",
						Title:         "Memory",
						DatasourceRef: ir.SelectedDatasourceRef,
						Query:         ir.QueryIR{Expr: `avg(process_resident_memory_bytes{service="checkout-api"})`},
						Unit:          "bytes",
						Layout:        ir.LayoutHint{Row: 0, Col: 0, Width: "full"},
					},
				},
			}
			imp, err := mgr2.Import(alice, jobs.ImportInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				Draft:         gaugeDraft,
				ImportSource:  &ir.ImportSource{UID: "orig-uid", ResourceVersion: "7"},
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("Import on replica 2 failed: %v", err)
			}

			spoofLLM := &stubLLM{
				onPatch: func(_ context.Context, req llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
					patch := ir.DashboardPatch{
						SchemaVersion: ir.SchemaVersion,
						BaseRevision:  req.CurrentIR.Revision,
						Summary:       "Attempt rate on gauge",
						Operations: []ir.PatchOperation{
							{
								Op:      "update_panel",
								PanelID: "panel-mem",
								Panel: &ir.PanelUpdate{
									Query: &ir.QueryUpdate{
										Expr: `rate(process_resident_memory_bytes{service="checkout-api"}[5m])`,
									},
								},
							},
						},
					}
					b, _ := json.Marshal(patch)
					return b, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
				},
			}
			mgrSpoof := jobs.NewManager(spoofLLM, cfg)
			defer mgrSpoof.Close()

			if _, err := mgrSpoof.Patch(context.Background(), alice, imp.JobID, jobs.PatchInput{
				BaseRevision: 1,
				EditIntent:   "rate on memory",
				Catalog:      []ir.MetricCatalogItem{{Name: "process_resident_memory_bytes", Type: "counter"}},
			}); err == nil {
				t.Fatal("expected cross-replica gauge->counter spoof to be rejected")
			}
		})
	}
}

func TestJobStore_StrictOwnerIsolationAcrossReplicas(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			cfg := jobs.Config{
				AllowedDSUIDs: []string{"prom-main"},
				Store:         store,
			}
			mgr1 := jobs.NewManager(&llm.MockClient{}, cfg)
			mgr2 := jobs.NewManager(&llm.MockClient{}, cfg)
			defer mgr1.Close()
			defer mgr2.Close()

			alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			intruders := []jobs.Owner{
				{OrgID: 1, UserLogin: "bob"},
				{OrgID: 2, UserLogin: "alice"},
				{OrgID: 1, UserLogin: "alice:admin"},
				{OrgID: 1, UserLogin: "alice\x00"},
			}

			snap, err := mgr1.Generate(context.Background(), alice, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate and error rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("alice Generate: %v", err)
			}
			panel := snap.CurrentIR.Panels[0]

			for _, intruder := range intruders {
				if _, err := mgr2.Get(intruder, snap.JobID); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v Get: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := mgr2.Patch(context.Background(), intruder, snap.JobID, jobs.PatchInput{
					BaseRevision: 1,
					EditIntent:   "hijack",
					Catalog:      testCatalog(),
				}); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v Patch: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := mgr2.Repair(context.Background(), intruder, snap.JobID, jobs.RepairInput{
					BaseRevision: 1,
					Results: []ir.PanelValidationResult{
						{PanelID: panel.ID, QueryHash: ir.PanelQueryHash(panel), Category: ir.ValidationSyntaxError},
					},
					Catalog: testCatalog(),
				}); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v Repair: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := mgr2.Undo(intruder, snap.JobID); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v Undo: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := mgr2.Cancel(intruder, snap.JobID); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v Cancel: expected ErrForbidden, got %v", intruder, err)
				}

				// Direct JobStore write methods MUST also enforce owner isolation.
				now := time.Now()
				if err := store.SetStatus(context.Background(), intruder, snap.JobID, 1, jobs.StatusFailed, now, time.Minute); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v store.SetStatus: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := store.FinishJob(context.Background(), jobs.FinishJobParams{
					Owner: intruder, JobID: snap.JobID, OpToken: 1, Status: jobs.StatusFailed, Now: now, JobTTL: time.Minute,
				}); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v store.FinishJob: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := store.CommitRevision(context.Background(), jobs.CommitRevisionParams{
					Owner: intruder, JobID: snap.JobID, OpToken: 1, ExpectedRev: 1, NextIR: *snap.CurrentIR, Now: now, JobTTL: time.Minute,
				}); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v store.CommitRevision: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := store.RecordEditFailure(context.Background(), jobs.RecordEditFailureParams{
					Owner: intruder, JobID: snap.JobID, OpToken: 1, ErrorMessage: "hijack", Now: now, JobTTL: time.Minute,
				}); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v store.RecordEditFailure: expected ErrForbidden, got %v", intruder, err)
				}
				if _, err := store.RevertOnCancel(context.Background(), intruder, snap.JobID, 1, now, time.Minute); !errors.Is(err, jobs.ErrForbidden) {
					t.Fatalf("intruder %+v store.RevertOnCancel: expected ErrForbidden, got %v", intruder, err)
				}
			}

			// Alice's job must be untouched.
			after, err := mgr2.Get(alice, snap.JobID)
			if err != nil {
				t.Fatalf("alice Get after intrusion attempts: %v", err)
			}
			if after.Status != jobs.StatusAwaitingPreview || after.RepairAttemptsUsed != 0 || after.CurrentIR.Revision != 1 {
				t.Fatalf("alice job was modified by unauthorized caller: %+v", after)
			}
		})
	}
}

func TestJobStore_PerUserAndGlobalConcurrencyLocking_Race(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			var activeGlobal atomic.Int64
			var maxObservedGlobal atomic.Int64
			var userMu sync.Mutex
			activeByUser := make(map[string]int)
			maxByUser := make(map[string]int)

			slowLLM := &stubLLM{
				onGenerate: func(ctx context.Context, req llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
					curGlobal := activeGlobal.Add(1)
					for {
						prev := maxObservedGlobal.Load()
						if curGlobal <= prev || maxObservedGlobal.CompareAndSwap(prev, curGlobal) {
							break
						}
					}

					userTag := req.UserIntent
					userMu.Lock()
					activeByUser[userTag]++
					if activeByUser[userTag] > maxByUser[userTag] {
						maxByUser[userTag] = activeByUser[userTag]
					}
					userMu.Unlock()

					time.Sleep(35 * time.Millisecond)

					userMu.Lock()
					activeByUser[userTag]--
					userMu.Unlock()
					activeGlobal.Add(-1)

					return (&llm.MockClient{}).GenerateIR(ctx, req)
				},
			}

			cfg := jobs.Config{
				AllowedDSUIDs:            []string{"prom-main"},
				MaxConcurrentJobs:        2,
				MaxConcurrentJobsPerUser: 1,
				Store:                    store,
			}

			replicas := []*jobs.Manager{
				jobs.NewManager(slowLLM, cfg),
				jobs.NewManager(slowLLM, cfg),
				jobs.NewManager(slowLLM, cfg),
			}
			for _, m := range replicas {
				defer m.Close()
			}

			users := []jobs.Owner{
				{OrgID: 1, UserLogin: "u1"},
				{OrgID: 1, UserLogin: "u2"},
				{OrgID: 1, UserLogin: "u3"},
				{OrgID: 2, UserLogin: "u1"},
			}

			var wg sync.WaitGroup
			var succeeded atomic.Int64
			var limited atomic.Int64
			var unexpectedErr atomic.Int64
			start := make(chan struct{})

			// Launch 3 concurrent requests per user spread across all 3 replicas (12 total).
			for i, u := range users {
				for attempt := 0; attempt < 3; attempt++ {
					wg.Add(1)
					replica := replicas[(i+attempt)%len(replicas)]
					owner := u
					go func() {
						defer wg.Done()
						<-start
						_, err := replica.Generate(context.Background(), owner, jobs.GenerateInput{
							DatasourceUID: "prom-main",
							Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
							UserIntent:    fmt.Sprintf("org%d:%s", owner.OrgID, owner.UserLogin),
							Catalog:       testCatalog(),
						})
						switch {
						case err == nil:
							succeeded.Add(1)
						case errors.Is(err, jobs.ErrConcurrencyLimited):
							limited.Add(1)
						default:
							unexpectedErr.Add(1)
							t.Errorf("unexpected error for %+v: %v", owner, err)
						}
					}()
				}
			}
			close(start)
			wg.Wait()

			if unexpectedErr.Load() != 0 {
				t.Fatalf("got %d unexpected errors", unexpectedErr.Load())
			}
			if succeeded.Load() == 0 || limited.Load() == 0 {
				t.Fatalf("expected mix of successes and concurrency limits, got succeeded=%d limited=%d", succeeded.Load(), limited.Load())
			}
			if maxObservedGlobal.Load() > 2 {
				t.Fatalf("global concurrency cap violated across replicas: observed=%d > 2", maxObservedGlobal.Load())
			}
			for userTag, maxSeen := range maxByUser {
				if maxSeen > 1 {
					t.Fatalf("per-user concurrency cap violated for %s across replicas: observed=%d > 1", userTag, maxSeen)
				}
			}

			// After all in-flight operations finish, slots MUST be cleanly released on every replica.
			for _, u := range users {
				if _, err := replicas[0].Generate(context.Background(), u, jobs.GenerateInput{
					DatasourceUID: "prom-main",
					Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
					UserIntent:    fmt.Sprintf("post-release-%d-%s", u.OrgID, u.UserLogin),
					Catalog:       testCatalog(),
				}); err != nil {
					t.Fatalf("expected released slot for %+v, got %v", u, err)
				}
			}
		})
	}
}

func TestJobStore_AtomicRepairBudgetAccountingAcrossReplicas_Race(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			var repairCalls atomic.Int64
			stub := &stubLLM{
				onGenerate: (&llm.MockClient{}).GenerateIR,
				onRepair: func(_ context.Context, req llm.RepairRequest) ([]byte, llm.CompletionMeta, error) {
					n := repairCalls.Add(1)
					time.Sleep(25 * time.Millisecond)
					win := "1m"
					if n%2 == 0 {
						win = "2m"
					}
					patch := ir.DashboardPatch{
						SchemaVersion: ir.SchemaVersion,
						BaseRevision:  req.CurrentIR.Revision,
						Summary:       fmt.Sprintf("Repair #%d", n),
						Operations: []ir.PatchOperation{
							{
								Op:      "update_panel",
								PanelID: req.Targets[0].PanelID,
								Panel: &ir.PanelUpdate{
									Query: &ir.QueryUpdate{
										Expr: fmt.Sprintf(`sum(rate(http_requests_total{service="checkout-api"}[%s]))`, win),
									},
								},
							},
						},
					}
					b, _ := json.Marshal(patch)
					return b, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
				},
			}

			// High concurrency limits so repair atomicity is stressed directly against per-job locking & budget.
			cfg := jobs.Config{
				AllowedDSUIDs:            []string{"prom-main"},
				MaxRepairAttempts:        2,
				MaxRepairAttemptsSet:     true,
				MaxConcurrentJobs:        16,
				MaxConcurrentJobsPerUser: 16,
				Store:                    store,
			}
			mgr1 := jobs.NewManager(stub, cfg)
			mgr2 := jobs.NewManager(stub, cfg)
			defer mgr1.Close()
			defer mgr2.Close()

			owner := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			snap, err := mgr1.Generate(context.Background(), owner, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			// Phase A: 10 concurrent Repair requests across Replica 1 & Replica 2 on rev 1.
			p1 := snap.CurrentIR.Panels[0]
			qHash1 := ir.PanelQueryHash(p1)
			var wg sync.WaitGroup
			var okCount, busyOrConflict atomic.Int64
			start := make(chan struct{})
			for i := 0; i < 10; i++ {
				wg.Add(1)
				m := mgr1
				if i%2 == 1 {
					m = mgr2
				}
				go func() {
					defer wg.Done()
					<-start
					_, rErr := m.Repair(context.Background(), owner, snap.JobID, jobs.RepairInput{
						BaseRevision: 1,
						Results: []ir.PanelValidationResult{
							{PanelID: p1.ID, QueryHash: qHash1, Category: ir.ValidationSyntaxError, ErrorText: "parse error"},
						},
						Catalog: testCatalog(),
					})
					switch {
					case rErr == nil:
						okCount.Add(1)
					case errors.Is(rErr, jobs.ErrOperationInProgress), errors.Is(rErr, ir.ErrRevisionConflict):
						busyOrConflict.Add(1)
					default:
						t.Errorf("unexpected parallel repair error: %v", rErr)
					}
				}()
			}
			close(start)
			wg.Wait()

			if okCount.Load() != 1 || busyOrConflict.Load() != 9 {
				t.Fatalf("expected exactly 1 winning repair and 9 rejected, got ok=%d rejected=%d", okCount.Load(), busyOrConflict.Load())
			}
			if repairCalls.Load() != 1 {
				t.Fatalf("expected exactly 1 LLM repair call, got %d", repairCalls.Load())
			}
			after1, err := mgr2.Get(owner, snap.JobID)
			if err != nil || after1.RepairAttemptsUsed != 1 || after1.CurrentIR.Revision != 2 {
				t.Fatalf("expected RepairAttemptsUsed=1 rev=2 after phase A, got err=%v snap=%+v", err, after1)
			}

			// Phase B: Second repair executed on Replica 2 (rev 2 -> rev 3).
			p2 := after1.CurrentIR.Panels[0]
			after2, err := mgr2.Repair(context.Background(), owner, snap.JobID, jobs.RepairInput{
				BaseRevision: 2,
				Results: []ir.PanelValidationResult{
					{PanelID: p2.ID, QueryHash: ir.PanelQueryHash(p2), Category: ir.ValidationSyntaxError, ErrorText: "parse error 2"},
				},
				Catalog: testCatalog(),
			})
			if err != nil {
				t.Fatalf("second repair on replica 2 failed: %v", err)
			}
			if after2.RepairAttemptsUsed != 2 || after2.CurrentIR.Revision != 3 {
				t.Fatalf("expected RepairAttemptsUsed=2 rev=3, got %+v", after2)
			}

			// Phase C: Concurrent Repair attempts across both replicas when budget is exhausted MUST all fail with ErrBudgetExhausted.
			p3 := after2.CurrentIR.Panels[0]
			qHash3 := ir.PanelQueryHash(p3)
			var exhausted atomic.Int64
			start2 := make(chan struct{})
			for i := 0; i < 6; i++ {
				wg.Add(1)
				m := mgr1
				if i%2 == 1 {
					m = mgr2
				}
				go func() {
					defer wg.Done()
					<-start2
					_, rErr := m.Repair(context.Background(), owner, snap.JobID, jobs.RepairInput{
						BaseRevision: 3,
						Results: []ir.PanelValidationResult{
							{PanelID: p3.ID, QueryHash: qHash3, Category: ir.ValidationSyntaxError, ErrorText: "parse error 3"},
						},
						Catalog: testCatalog(),
					})
					if errors.Is(rErr, jobs.ErrBudgetExhausted) {
						exhausted.Add(1)
					} else {
						t.Errorf("expected ErrBudgetExhausted, got %v", rErr)
					}
				}()
			}
			close(start2)
			wg.Wait()

			if exhausted.Load() != 6 || repairCalls.Load() != 2 {
				t.Fatalf("expected all 6 calls rejected with ErrBudgetExhausted and total LLM calls=2, got exhausted=%d calls=%d", exhausted.Load(), repairCalls.Load())
			}
		})
	}
}

func TestJobStore_CrossReplicaUndoAndCancelFencing_Race(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			patchStarted := make(chan struct{})
			patchRelease := make(chan struct{})
			stub1 := &stubLLM{
				onGenerate: (&llm.MockClient{}).GenerateIR,
				onPatch: func(ctx context.Context, req llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
					if req.CurrentIR.Revision == 1 {
						return (&llm.MockClient{}).GeneratePatch(ctx, req)
					}
					close(patchStarted)
					<-patchRelease
					// Return a valid patch AFTER Replica 2 already executed Undo.
					patch := ir.DashboardPatch{
						SchemaVersion: ir.SchemaVersion,
						BaseRevision:  req.CurrentIR.Revision,
						Summary:       "Stale replica 1 patch",
						Operations: []ir.PatchOperation{
							{
								Op:      "update_panel",
								PanelID: "panel-error-rate",
								Panel:   &ir.PanelUpdate{Title: "Stale Overwrite"},
							},
						},
					}
					b, _ := json.Marshal(patch)
					return b, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
				},
			}

			cfg := jobs.Config{AllowedDSUIDs: []string{"prom-main"}, Store: store}
			mgr1 := jobs.NewManager(stub1, cfg)
			mgr2 := jobs.NewManager(&llm.MockClient{}, cfg)
			defer mgr1.Close()
			defer mgr2.Close()

			owner := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			s1, err := mgr1.Generate(context.Background(), owner, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate and error rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if _, err := mgr1.Patch(context.Background(), owner, s1.JobID, jobs.PatchInput{
				BaseRevision:  1,
				TargetPanelID: "panel-error-rate",
				EditIntent:    "Switch error panel to timeseries",
				Catalog:       testCatalog(),
			}); err != nil {
				t.Fatalf("first patch: %v", err)
			}

			// Start slow Patch on Replica 1.
			lateErrCh := make(chan error, 1)
			go func() {
				_, e := mgr1.Patch(context.Background(), owner, s1.JobID, jobs.PatchInput{
					BaseRevision:  2,
					TargetPanelID: "panel-error-rate",
					EditIntent:    "Rename panel",
					Catalog:       testCatalog(),
				})
				lateErrCh <- e
			}()

			<-patchStarted
			// Replica 2 runs Undo while Replica 1 is mid-LLM call.
			undoSnap, err := mgr2.Undo(owner, s1.JobID)
			if err != nil || undoSnap.CurrentIR.Revision != 3 || undoSnap.Status != jobs.StatusAwaitingPreview {
				t.Fatalf("replica 2 Undo failed: err=%v snap=%+v", err, undoSnap)
			}

			// Unblock Replica 1's late patch result -> MUST be fenced with ErrJobCancelled!
			close(patchRelease)
			lateErr := <-lateErrCh
			if !errors.Is(lateErr, jobs.ErrJobCancelled) {
				t.Fatalf("expected stale replica 1 patch to fail with ErrJobCancelled, got %v", lateErr)
			}

			finalSnap, err := mgr2.Get(owner, s1.JobID)
			if err != nil || finalSnap.CurrentIR.Revision != 3 || finalSnap.Status != jobs.StatusAwaitingPreview {
				t.Fatalf("stale replica 1 patch corrupted post-Undo state: err=%v snap=%+v", err, finalSnap)
			}
			for _, p := range finalSnap.CurrentIR.Panels {
				if p.Title == "Stale Overwrite" {
					t.Fatalf("stale replica 1 panel title leaked into post-Undo draft")
				}
			}
		})
	}
}

func TestJobStore_LeaseExpirationRecoveryAndTTL(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			clk := newTestClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
			cfg := jobs.Config{
				AllowedDSUIDs:            []string{"prom-main"},
				JobTTL:                   2 * time.Minute,
				OpLeaseTTL:               10 * time.Second,
				MaxConcurrentJobs:        2,
				MaxConcurrentJobsPerUser: 1,
				MaxJobs:                  2,
				MaxJobsPerUser:           2,
				Store:                    store,
				NowFunc:                  clk.Now,
			}
			mgr1 := jobs.NewManager(&llm.MockClient{}, cfg)
			mgr2 := jobs.NewManager(&llm.MockClient{}, cfg)
			defer mgr1.Close()
			defer mgr2.Close()

			alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			mallory := jobs.Owner{OrgID: 1, UserLogin: "mallory"}

			snap, err := mgr1.Generate(context.Background(), alice, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate and error rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			// Simulate a crashed replica acquiring BeginPatch lease + slot and never releasing it.
			_, err = store.BeginPatch(context.Background(), jobs.BeginPatchParams{
				Owner:        alice,
				JobID:        snap.JobID,
				BaseRevision: 1,
				SlotID:       "crashed-replica-slot",
				Now:          clk.Now(),
				Limits: jobs.StoreLimits{
					JobTTL:                   cfg.JobTTL,
					OpLeaseTTL:               cfg.OpLeaseTTL,
					MaxConcurrentJobs:        cfg.MaxConcurrentJobs,
					MaxConcurrentJobsPerUser: cfg.MaxConcurrentJobsPerUser,
				},
			})
			if err != nil {
				t.Fatalf("simulated crashed BeginPatch: %v", err)
			}

			// Before lease expires: Replica 2 is blocked by ErrOperationInProgress.
			if _, err := mgr2.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
				BaseRevision: 1,
				EditIntent:   "Try before lease expiry",
				Catalog:      testCatalog(),
			}); !errors.Is(err, jobs.ErrOperationInProgress) {
				t.Fatalf("expected ErrOperationInProgress before lease expiry, got %v", err)
			}

			// Advance past OpLeaseTTL (10s) but within JobTTL (2m): Replica 2 recovers cleanly!
			clk.Advance(11 * time.Second)
			patched, err := mgr2.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
				BaseRevision: 1,
				EditIntent:   "Switch error panel to timeseries",
				Catalog:      testCatalog(),
			})
			if err != nil {
				t.Fatalf("expected recovery after lease expiry, got %v", err)
			}
			if patched.CurrentIR.Revision != 2 {
				t.Fatalf("expected rev 2 after recovery, got %d", patched.CurrentIR.Revision)
			}

			// Cross-user Import eviction isolation: Alice fills MaxJobs=2, Mallory Import MUST NOT evict Alice.
			if _, err := mgr1.Import(alice, jobs.ImportInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				Draft:         *patched.CurrentIR,
				Catalog:       testCatalog(),
			}); err != nil {
				t.Fatalf("alice second job failed: %v", err)
			}
			if _, err := mgr2.Import(mallory, jobs.ImportInput{
				DatasourceUID: "prom-main",
				Draft:         *patched.CurrentIR,
			}); !errors.Is(err, jobs.ErrConcurrencyLimited) {
				t.Fatalf("expected ErrConcurrencyLimited preventing cross-user eviction, got %v", err)
			}

			// Advance past JobTTL (2m) -> CleanupExpired and Get reflect automatic expiration.
			clk.Advance(3 * time.Minute)
			if err := mgr2.CleanupExpired(context.Background()); err != nil {
				t.Fatalf("CleanupExpired: %v", err)
			}
			if _, err := mgr2.Get(alice, snap.JobID); !errors.Is(err, jobs.ErrJobNotFound) {
				t.Fatalf("expected ErrJobNotFound after JobTTL expiry, got %v", err)
			}
		})
	}
}

func TestRedisStore_NativeTTLExpirationValidationAndPingFailure(t *testing.T) {
	// Config validation checks.
	if _, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: ""}); err == nil {
		t.Fatal("expected empty redisAddr to fail")
	}
	if _, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: "redis://user:pass@localhost:6379"}); err == nil {
		t.Fatal("expected URL scheme / credentials in redisAddr to fail")
	}
	if _, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: "localhost:99999"}); err == nil {
		t.Fatal("expected out-of-range port to fail")
	}
	if _, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: "localhost:6379", DB: 16}); err == nil {
		t.Fatal("expected redisDb=16 to fail")
	}
	if _, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: "localhost:6379", KeyPrefix: "bad prefix!"}); err == nil {
		t.Fatal("expected invalid redisKeyPrefix to fail")
	}

	mr := miniredis.RunT(t)
	store, err := jobs.NewRedisStore(jobs.RedisConfig{
		Addr:      mr.Addr(),
		KeyPrefix: "ttltest:jobs",
	})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on live miniredis failed: %v", err)
	}

	mgr := jobs.NewManager(&llm.MockClient{}, jobs.Config{
		AllowedDSUIDs: []string{"prom-main"},
		JobTTL:        250 * time.Millisecond,
		Store:         store,
	})
	defer mgr.Close()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	snap, err := mgr.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Show request rate",
		Catalog:       testCatalog(),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	redisKey := "{ttltest:jobs}:job:" + snap.JobID
	if !mr.Exists(redisKey) {
		t.Fatalf("expected Redis key %s to exist", redisKey)
	}
	if ttl := mr.TTL(redisKey); ttl <= 0 || ttl > time.Second {
		t.Fatalf("expected bounded positive Redis PEXPIRE on %s, got %v", redisKey, ttl)
	}

	// Ensure user login PII is not exposed in Redis key names.
	for _, k := range mr.Keys() {
		if strings.Contains(k, "alice") {
			t.Fatalf("expected no raw UserLogin PII in Redis key names, found %q", k)
		}
	}

	// Fast-forward miniredis's native TTL clock -> Redis itself evicts the key without Go clock tricks.
	mr.FastForward(500 * time.Millisecond)
	if mr.Exists(redisKey) {
		t.Fatalf("expected native Redis TTL to evict %s", redisKey)
	}
	if _, err := mgr.Get(alice, snap.JobID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("expected ErrJobNotFound after native Redis TTL eviction, got %v", err)
	}

	// Stop Redis -> Ping and Get MUST return ErrStoreUnavailable.
	mr.Close()
	if err := store.Ping(context.Background()); !errors.Is(err, jobs.ErrStoreUnavailable) {
		t.Fatalf("expected ErrStoreUnavailable on Ping after Redis shutdown, got %v", err)
	}
	if _, err := mgr.Get(alice, snap.JobID); !errors.Is(err, jobs.ErrStoreUnavailable) {
		t.Fatalf("expected ErrStoreUnavailable on Get after Redis shutdown, got %v", err)
	}
}
