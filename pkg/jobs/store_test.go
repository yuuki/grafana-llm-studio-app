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
				{OrgID: 1, UserLogin: "u4"},
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

func TestJobStore_MultiOrgIsolation(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			org2Started := make(chan struct{}, 2)
			org2Release := make(chan struct{})
			llmStub := &stubLLM{
				onGenerate: func(ctx context.Context, req llm.GenerateRequest) ([]byte, llm.CompletionMeta, error) {
					if req.UserIntent == "org2-hold" {
						org2Started <- struct{}{}
						<-org2Release
					}
					return (&llm.MockClient{}).GenerateIR(ctx, req)
				},
				onPatch: (&llm.MockClient{}).GeneratePatch,
			}

			cfg := jobs.Config{
				AllowedDSUIDs:            []string{"prom-main"},
				MaxJobs:                  1,
				MaxJobsPerUser:           1,
				MaxConcurrentJobs:        1,
				MaxConcurrentJobsPerUser: 1,
				Store:                    store,
			}
			mgr1 := jobs.NewManager(llmStub, cfg)
			mgr2 := jobs.NewManager(llmStub, cfg)
			defer mgr1.Close()
			defer mgr2.Close()

			org1Alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			org2Bob := jobs.Owner{OrgID: 2, UserLogin: "bob"}

			// Org 1 creates a job (fills Org 1's MaxJobs=1).
			org1Job, err := mgr1.Generate(context.Background(), org1Alice, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate and error rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("org1 Generate: %v", err)
			}

			// Org 2 holds its full concurrency slot (MaxConcurrentJobs=1) and creates a job (MaxJobs=1).
			doneOrg2 := make(chan error, 1)
			go func() {
				_, gErr := mgr2.Generate(context.Background(), org2Bob, jobs.GenerateInput{
					DatasourceUID: "prom-main",
					Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
					UserIntent:    "org2-hold",
					Catalog:       testCatalog(),
				})
				doneOrg2 <- gErr
			}()
			<-org2Started

			// Org 1 MUST NOT be starved by Org 2's concurrency slot AND Org 1's job MUST NOT be evicted by Org 2!
			if _, err := mgr1.Get(org1Alice, org1Job.JobID); err != nil {
				t.Fatalf("org1 job was evicted by org2: %v", err)
			}
			if _, err := mgr1.Patch(context.Background(), org1Alice, org1Job.JobID, jobs.PatchInput{
				BaseRevision:  1,
				TargetPanelID: "panel-error-rate",
				EditIntent:    "Switch error panel to timeseries",
				Catalog:       testCatalog(),
			}); err != nil {
				t.Fatalf("org1 Patch failed while org2 held org2 slot: %v", err)
			}

			close(org2Release)
			if err := <-doneOrg2; err != nil {
				t.Fatalf("org2 Generate failed: %v", err)
			}
			// Org 1's job MUST still exist after Org 2 finished.
			if _, err := mgr1.Get(org1Alice, org1Job.JobID); err != nil {
				t.Fatalf("org1 job missing after org2 completed: %v", err)
			}
		})
	}
}

func TestRedisStore_CrossReplicaCancelPubSubUnwindsSlotImmediately(t *testing.T) {
	mr := miniredis.RunT(t)
	store1, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: mr.Addr(), KeyPrefix: "pubsubtest"})
	if err != nil {
		t.Fatalf("NewRedisStore 1: %v", err)
	}
	defer func() { _ = store1.Close() }()
	store2, err := jobs.NewRedisStore(jobs.RedisConfig{Addr: mr.Addr(), KeyPrefix: "pubsubtest"})
	if err != nil {
		t.Fatalf("NewRedisStore 2: %v", err)
	}
	defer func() { _ = store2.Close() }()

	patchStarted := make(chan struct{})
	cancelledInLLM := make(chan struct{})
	stub1 := &stubLLM{
		onGenerate: (&llm.MockClient{}).GenerateIR,
		onPatch: func(ctx context.Context, _ llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
			close(patchStarted)
			<-ctx.Done()
			close(cancelledInLLM)
			return nil, llm.CompletionMeta{ProviderMode: "stub"}, ctx.Err()
		},
	}

	mgr1 := jobs.NewManager(stub1, jobs.Config{
		AllowedDSUIDs:            []string{"prom-main"},
		MaxConcurrentJobs:        1,
		MaxConcurrentJobsPerUser: 1,
		JobDeadline:              60 * time.Second,
		Store:                    store1,
	})
	defer mgr1.Close()
	mgr2 := jobs.NewManager(&llm.MockClient{}, jobs.Config{
		AllowedDSUIDs:            []string{"prom-main"},
		MaxConcurrentJobs:        1,
		MaxConcurrentJobsPerUser: 1,
		JobDeadline:              60 * time.Second,
		Store:                    store2,
	})
	defer mgr2.Close()

	alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
	s1, err := mgr1.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Show request rate and error rate",
		Catalog:       testCatalog(),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	patchDone := make(chan error, 1)
	go func() {
		_, pErr := mgr1.Patch(context.Background(), alice, s1.JobID, jobs.PatchInput{
			BaseRevision:  1,
			TargetPanelID: "panel-error-rate",
			EditIntent:    "Slow edit",
			Catalog:       testCatalog(),
		})
		patchDone <- pErr
	}()
	<-patchStarted

	// Cancel arrives at Replica 2 -> Pub/Sub MUST immediately cancel Replica 1's LLM context and release the slot!
	if _, err := mgr2.Cancel(alice, s1.JobID); err != nil {
		t.Fatalf("Replica 2 Cancel failed: %v", err)
	}

	select {
	case <-cancelledInLLM:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cross-replica Pub/Sub to cancel Replica 1 LLM context")
	}
	if pErr := <-patchDone; !errors.Is(pErr, jobs.ErrJobCancelled) {
		t.Fatalf("expected Replica 1 Patch to return ErrJobCancelled, got %v", pErr)
	}

	// Slot is immediately free on Replica 2 (no 60-75s lockout!).
	if _, err := mgr2.Generate(context.Background(), alice, jobs.GenerateInput{
		DatasourceUID: "prom-main",
		Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
		UserIntent:    "Fresh generation after cross-replica cancel",
		Catalog:       testCatalog(),
	}); err != nil {
		t.Fatalf("expected immediate Generate on Replica 2 after cross-replica cancel, got %v", err)
	}
}

func TestJobStore_AdversarialFencingNewlineInjectionHistoryAndErrors(t *testing.T) {
	for _, sf := range allStoreFactories() {
		t.Run(sf.name, func(t *testing.T) {
			store, cleanup := sf.new(t)
			defer cleanup()

			clk := newTestClock(time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC))
			var llmPatchCalls atomic.Int64
			var patchErrToReturn error
			stub := &stubLLM{
				onGenerate: (&llm.MockClient{}).GenerateIR,
				onPatch: func(ctx context.Context, req llm.PatchRequest) ([]byte, llm.CompletionMeta, error) {
					llmPatchCalls.Add(1)
					if patchErrToReturn != nil {
						return nil, llm.CompletionMeta{ProviderMode: "stub"}, patchErrToReturn
					}
					patch := ir.DashboardPatch{
						SchemaVersion: ir.SchemaVersion,
						BaseRevision:  req.CurrentIR.Revision,
						Summary:       fmt.Sprintf("Title rev %d", req.CurrentIR.Revision+1),
						Operations: []ir.PatchOperation{
							{
								Op:      "update_panel",
								PanelID: "panel-error-rate",
								Panel:   &ir.PanelUpdate{Title: fmt.Sprintf("Errors v%d", req.CurrentIR.Revision+1)},
							},
						},
					}
					b, _ := json.Marshal(patch)
					return b, llm.CompletionMeta{FinishReason: "stop", ProviderMode: "stub"}, nil
				},
			}

			mgr := jobs.NewManager(stub, jobs.Config{
				AllowedDSUIDs: []string{"prom-main"},
				JobTTL:        5 * time.Minute,
				OpLeaseTTL:    10 * time.Second,
				Store:         store,
				NowFunc:       clk.Now,
			})
			defer mgr.Close()

			alice := jobs.Owner{OrgID: 1, UserLogin: "alice"}
			snap, err := mgr.Generate(context.Background(), alice, jobs.GenerateInput{
				DatasourceUID: "prom-main",
				Scope:         &ir.ScopeFilter{LabelName: "service", LabelValue: "checkout-api"},
				UserIntent:    "Show request rate and error rate",
				Catalog:       testCatalog(),
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			// H-8: TargetPanelID with embedded newline MUST be rejected with ErrPanelNotFound before LLM call.
			if _, err := mgr.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
				BaseRevision:  1,
				TargetPanelID: "panel-request-rate\npanel-error-rate",
				EditIntent:    "newline injection",
				Catalog:       testCatalog(),
			}); !errors.Is(err, ir.ErrPanelNotFound) {
				t.Fatalf("expected ErrPanelNotFound for newline TargetPanelID, got %v", err)
			}
			if llmPatchCalls.Load() != 0 {
				t.Fatalf("LLM should not have been called on newline TargetPanelID, calls=%d", llmPatchCalls.Load())
			}

			// M-8: Invalid JobIDs rejected cleanly.
			for _, badID := range []string{"", "job:evil", "../secret", "job with space", "job*"} {
				if _, err := mgr.Get(alice, badID); !errors.Is(err, jobs.ErrJobNotFound) {
					t.Fatalf("expected ErrJobNotFound for bad jobID %q, got %v", badID, err)
				}
			}

			// H-5: Replaying FinishJob / RecordEditFailure with old token after completion MUST return ErrJobCancelled.
			if _, err := store.FinishJob(context.Background(), jobs.FinishJobParams{
				Owner: alice, JobID: snap.JobID, OpToken: 1, Status: jobs.StatusFailed, ErrorMessage: "replay", Now: clk.Now(), JobTTL: time.Minute,
			}); !errors.Is(err, jobs.ErrJobCancelled) {
				t.Fatalf("expected replayed FinishJob after completion to return ErrJobCancelled, got %v", err)
			}
			if _, err := store.RecordEditFailure(context.Background(), jobs.RecordEditFailureParams{
				Owner: alice, JobID: snap.JobID, OpToken: 2, ErrorMessage: "inactive-replay", Now: clk.Now(), JobTTL: time.Minute,
			}); !errors.Is(err, jobs.ErrJobCancelled) {
				t.Fatalf("expected RecordEditFailure on idle job to return ErrJobCancelled, got %v", err)
			}

			// H-5: After OpLeaseTTL expires, late CommitRevision from the stalled op MUST return ErrJobCancelled.
			opRes, err := store.BeginPatch(context.Background(), jobs.BeginPatchParams{
				Owner: alice, JobID: snap.JobID, BaseRevision: 1, SlotID: "stalled-slot", Now: clk.Now(),
				Limits: jobs.StoreLimits{JobTTL: 5 * time.Minute, OpLeaseTTL: 10 * time.Second},
			})
			if err != nil {
				t.Fatalf("BeginPatch: %v", err)
			}
			clk.Advance(15 * time.Second)
			nextDoc := opRes.CurrentIR.Clone()
			nextDoc.Revision = 2
			if _, err := store.CommitRevision(context.Background(), jobs.CommitRevisionParams{
				Owner: alice, JobID: snap.JobID, OpToken: opRes.OpToken, ExpectedRev: 1, NextIR: nextDoc, Now: clk.Now(), JobTTL: 5 * time.Minute,
			}); !errors.Is(err, jobs.ErrJobCancelled) {
				t.Fatalf("expected late CommitRevision after lease expiry to return ErrJobCancelled, got %v", err)
			}
			_ = store.ReleaseSlot(context.Background(), alice, "stalled-slot", clk.Now())

			// M-4: Error causes (context.DeadlineExceeded, llm.ErrOutputTruncated) MUST survive RecordEditFailure.
			patchErrToReturn = context.DeadlineExceeded
			if _, err := mgr.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
				BaseRevision: 1, EditIntent: "timeout test", Catalog: testCatalog(),
			}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected context.DeadlineExceeded preserved, got %v", err)
			}
			patchErrToReturn = llm.ErrOutputTruncated
			if _, err := mgr.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
				BaseRevision: 1, EditIntent: "truncated test", Catalog: testCatalog(),
			}); !errors.Is(err, llm.ErrOutputTruncated) {
				t.Fatalf("expected llm.ErrOutputTruncated preserved, got %v", err)
			}
			patchErrToReturn = nil

			// M-5: Commit 12 revisions -> verify 10-deep undo stack, Undo -> Patch -> Undo, and MaxUndoHistory shrink.
			curRev := 1
			for i := 0; i < 12; i++ {
				clk.Advance(time.Second)
				pSnap, pErr := mgr.Patch(context.Background(), alice, snap.JobID, jobs.PatchInput{
					BaseRevision: curRev, TargetPanelID: "panel-error-rate", EditIntent: "step", Catalog: testCatalog(),
				})
				if pErr != nil {
					t.Fatalf("patch step %d failed: %v", i, pErr)
				}
				curRev = pSnap.CurrentIR.Revision
			}
			// Now shrink MaxUndoHistory from 10 -> 3 via direct CommitRevision and verify the 3 NEWEST revisions remain.
			shrinkOp, err := store.BeginPatch(context.Background(), jobs.BeginPatchParams{
				Owner: alice, JobID: snap.JobID, BaseRevision: curRev, SlotID: "shrink-slot", Now: clk.Now(),
				Limits: jobs.StoreLimits{JobTTL: 5 * time.Minute, OpLeaseTTL: 10 * time.Second},
			})
			if err != nil {
				t.Fatalf("shrink BeginPatch: %v", err)
			}
			shrunkDoc := shrinkOp.CurrentIR.Clone()
			shrunkDoc.Revision = curRev + 1
			shrunkSnap, err := store.CommitRevision(context.Background(), jobs.CommitRevisionParams{
				Owner: alice, JobID: snap.JobID, OpToken: shrinkOp.OpToken, ExpectedRev: curRev,
				NextIR: shrunkDoc, Now: clk.Now(), JobTTL: 5 * time.Minute, MaxUndoHistory: 3,
			})
			_ = store.ReleaseSlot(context.Background(), alice, "shrink-slot", clk.Now())
			if err != nil || shrunkSnap.CurrentIR.Revision != curRev+1 {
				t.Fatalf("shrink CommitRevision failed: err=%v snap=%+v", err, shrunkSnap)
			}
			// Expect exactly 3 undos restoring curRev, curRev-1, curRev-2 (newest, NOT oldest!).
			expectedRestored := []int{curRev, curRev - 1, curRev - 2}
			for idx, wantPrev := range expectedRestored {
				uSnap, uErr := mgr.Undo(alice, snap.JobID)
				if uErr != nil {
					t.Fatalf("undo %d failed: %v", idx, uErr)
				}
				wantSubstring := fmt.Sprintf("restored from rev %d as rev", wantPrev)
				if len(uSnap.LastDiff) != 1 || !strings.Contains(uSnap.LastDiff[0].After, wantSubstring) {
					t.Fatalf("undo %d restored wrong revision: got %+v, want %q", idx, uSnap.LastDiff, wantSubstring)
				}
			}
			if _, err := mgr.Undo(alice, snap.JobID); !errors.Is(err, jobs.ErrNoUndoHistory) {
				t.Fatalf("expected ErrNoUndoHistory after 3 undos on shrunk stack, got %v", err)
			}
		})
	}
}

func TestRedisStore_StrictAddrValidation(t *testing.T) {
	valid := []string{"localhost:6379", "redis.internal:6379", "127.0.0.1:6379", "[::1]:6379", "[2001:db8::1]:6380"}
	for _, addr := range valid {
		if err := jobs.ValidateRedisAddr(addr); err != nil {
			t.Fatalf("expected valid redisAddr %q to pass, got %v", addr, err)
		}
	}
	invalid := []string{
		"",
		"redis:0",
		"redis:65536",
		"redis:+6379",
		"redis:06379",
		"redis : 6379",
		"redis host:6379",
		"redis\r\n:6379",
		"redis/path:6379",
		"::1:6379",
		"[]:6379",
		"[::1:6379",
		"::1]:6379",
		"redis://localhost:6379",
		"user:pass@localhost:6379",
	}
	for _, addr := range invalid {
		if err := jobs.ValidateRedisAddr(addr); err == nil {
			t.Fatalf("expected invalid redisAddr %q to be rejected", addr)
		}
	}
}
