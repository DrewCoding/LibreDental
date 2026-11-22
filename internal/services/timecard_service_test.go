package services_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LibreDental/libredental/internal/domain"
	"github.com/LibreDental/libredental/internal/services"
	"github.com/LibreDental/libredental/internal/storage/sqlite"
)

func TestTimecardService_ValidationAndErrors(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_timecard_svc.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open sqlite db: %v", err)
	}
	defer db.Close()

	configRepo := sqlite.NewPracticeConfigRepository(db)
	timecardRepo := sqlite.NewTimecardRepository(db)
	service := services.NewTimecardService(timecardRepo, configRepo, nil)
	ctx := context.Background()

	prov := &domain.Provider{
		ID:         "prov_svc_1",
		Name:       "Dr. Service",
		Role:       domain.RoleDentist,
		HourlyRate: 75,
		IsActive:   true,
	}
	if err := configRepo.SaveProvider(ctx, prov); err != nil {
		t.Fatalf("Failed to save provider: %v", err)
	}

	t.Run("ClockIn duplicate active check", func(t *testing.T) {
		tc1, err := service.ClockIn("", "prov_svc_1")
		if err != nil {
			t.Fatalf("ClockIn failed: %v", err)
		}
		if tc1 == nil {
			t.Fatalf("Expected timecard on ClockIn, got nil")
		}

		// Second clock in should fail
		_, err = service.ClockIn("", "prov_svc_1")
		if err == nil {
			t.Fatalf("Expected error when clocking in twice, got nil")
		}

		// Clock out
		_, err = service.ClockOut("", "prov_svc_1")
		if err != nil {
			t.Fatalf("ClockOut failed: %v", err)
		}
	})

	t.Run("ListTimecards invalid date parsing", func(t *testing.T) {
		_, err := service.ListTimecards("", "prov_svc_1", "invalid-date", "")
		if err == nil {
			t.Errorf("Expected error for invalid start date, got nil")
		}

		_, err = service.ListTimecards("", "prov_svc_1", "", "invalid-date")
		if err == nil {
			t.Errorf("Expected error for invalid end date, got nil")
		}

		validDate := time.Now().Format(time.RFC3339)
		list, err := service.ListTimecards("", "prov_svc_1", validDate, validDate)
		if err != nil {
			t.Errorf("Expected success for valid dates, got %v", err)
		}
		if list == nil {
			t.Errorf("Expected non-nil slice from ListTimecards")
		}
	})

	t.Run("CreateManualTimecard validation", func(t *testing.T) {
		validDate := time.Now().Format(time.RFC3339)

		// Non-positive minutes
		err := service.CreateManualTimecard("", "prov_svc_1", 0, validDate)
		if err == nil {
			t.Errorf("Expected error for 0 minutes in CreateManualTimecard, got nil")
		}

		err = service.CreateManualTimecard("", "prov_svc_1", -30, validDate)
		if err == nil {
			t.Errorf("Expected error for negative minutes in CreateManualTimecard, got nil")
		}

		// Invalid date format
		err = service.CreateManualTimecard("", "prov_svc_1", 60, "not-a-date")
		if err == nil {
			t.Errorf("Expected error for invalid date format in CreateManualTimecard, got nil")
		}

		// Valid manual timecard
		err = service.CreateManualTimecard("", "prov_svc_1", 60, validDate)
		if err != nil {
			t.Errorf("Expected success for valid manual timecard, got %v", err)
		}
	})
}

func TestTimecardService_ClockRequiresSession(t *testing.T) {
	tempDir := t.TempDir()

	db, err := sqlite.Open(filepath.Join(tempDir, "main.db"))
	if err != nil {
		t.Fatalf("Failed to open sqlite db: %v", err)
	}
	defer db.Close()

	auditDb, err := sqlite.OpenAudit(filepath.Join(tempDir, "audit.db"))
	if err != nil {
		t.Fatalf("Failed to open audit sqlite db: %v", err)
	}
	defer auditDb.Close()

	configRepo := sqlite.NewPracticeConfigRepository(db)
	auditService := services.NewAuditService(sqlite.NewAuditRepository(auditDb), configRepo)
	service := services.NewTimecardService(sqlite.NewTimecardRepository(db), configRepo, auditService)

	if err := configRepo.SaveProvider(context.Background(), &domain.Provider{
		ID: "prov_clock", Name: "Dr. Clock", Role: domain.RoleDentist, Pin: "1234", IsActive: true,
	}); err != nil {
		t.Fatalf("Failed to save provider: %v", err)
	}

	if _, err := service.ClockIn("", "prov_clock"); !errors.Is(err, services.ErrUnauthorized) {
		t.Fatalf("Expected ClockIn without session to be unauthorized, got %v", err)
	}
	if _, err := service.ClockOut("", "prov_clock"); !errors.Is(err, services.ErrUnauthorized) {
		t.Fatalf("Expected ClockOut without session to be unauthorized, got %v", err)
	}
	if _, err := service.ClockIn("unknown-token", "prov_clock"); !errors.Is(err, services.ErrUnauthorized) {
		t.Fatalf("Expected ClockIn with unknown token to be unauthorized, got %v", err)
	}
	if _, err := service.ClockOut("unknown-token", "prov_clock"); !errors.Is(err, services.ErrUnauthorized) {
		t.Fatalf("Expected ClockOut with unknown token to be unauthorized, got %v", err)
	}

	token, err := auditService.CreateSession("prov_clock", "1234")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	if _, err := service.ClockIn(token, "prov_clock"); err != nil {
		t.Fatalf("Expected ClockIn with session to succeed, got %v", err)
	}
	if _, err := service.ClockOut(token, "prov_clock"); err != nil {
		t.Fatalf("Expected ClockOut with session to succeed, got %v", err)
	}
}

// Timecards show pay, hours and who is on site right now, so reading them needs a session,
// as in LAN server mode any client on the network can call these methods.
func TestTimecardService_ReadsRequireSession(t *testing.T) {
	tempDir := t.TempDir()

	db, err := sqlite.Open(filepath.Join(tempDir, "main.db"))
	if err != nil {
		t.Fatalf("Failed to open sqlite db: %v", err)
	}
	defer db.Close()

	auditDb, err := sqlite.OpenAudit(filepath.Join(tempDir, "audit.db"))
	if err != nil {
		t.Fatalf("Failed to open audit sqlite db: %v", err)
	}
	defer auditDb.Close()

	configRepo := sqlite.NewPracticeConfigRepository(db)
	auditService := services.NewAuditService(sqlite.NewAuditRepository(auditDb), configRepo)
	service := services.NewTimecardService(sqlite.NewTimecardRepository(db), configRepo, auditService)

	if err := configRepo.SaveProvider(context.Background(), &domain.Provider{
		ID: "prov_pay", Name: "Dr. Pay", Role: domain.RoleDentist, Pin: "1234", HourlyRate: 4250, IsActive: true,
	}); err != nil {
		t.Fatalf("Failed to save provider: %v", err)
	}
	token, err := auditService.CreateSession("prov_pay", "1234")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	if err := service.CreateManualTimecard(token, "prov_pay", 480, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatalf("Failed to create timecard: %v", err)
	}
	if _, err := service.ClockIn(token, "prov_pay"); err != nil {
		t.Fatalf("Failed to clock in: %v", err)
	}

	for _, bad := range []string{"", "unknown-token"} {
		if _, err := service.ListTimecards(bad, "all", "", ""); !errors.Is(err, services.ErrUnauthorized) {
			t.Errorf("Expected ListTimecards with token %q to be unauthorized, got %v", bad, err)
		}
		if _, err := service.GetActiveTimecard(bad, "prov_pay"); !errors.Is(err, services.ErrUnauthorized) {
			t.Errorf("Expected GetActiveTimecard with token %q to be unauthorized, got %v", bad, err)
		}
		if _, err := service.GetTotalOwed(bad, "prov_pay"); !errors.Is(err, services.ErrUnauthorized) {
			t.Errorf("Expected GetTotalOwed with token %q to be unauthorized, got %v", bad, err)
		}
	}

	if list, err := service.ListTimecards(token, "all", "", ""); err != nil || len(list) != 2 {
		t.Errorf("Expected 2 timecards with a session, got %d, %v", len(list), err)
	}
	if tc, err := service.GetActiveTimecard(token, "prov_pay"); err != nil || tc == nil {
		t.Errorf("Expected an active timecard with a session, got %v, %v", tc, err)
	}
	if owed, err := service.GetTotalOwed(token, "prov_pay"); err != nil || owed != 34000 {
		t.Errorf("Expected 34000 cents owed with a session, got %d, %v", owed, err)
	}
}
