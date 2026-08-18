package integrated

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// setupBaselineAuditReceipt is persisted in the same durable state object as a
// baseline phase transition. A transition is not consumable until its exact
// audit record is durably present and this receipt has been cleared. The audit
// scan makes recovery idempotent across the unavoidable crash window between
// appending audit.jsonl and clearing the receipt.
type setupBaselineAuditReceipt struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Detail  string `json:"detail"`
}

func validateSetupBaselineAuditReceipt(receipt *setupBaselineAuditReceipt) error {
	if receipt == nil {
		return nil
	}
	if len(receipt.ID) != 64 || strings.Trim(receipt.ID, "0123456789abcdef") != "" || strings.TrimSpace(receipt.Action) == "" || strings.TrimSpace(receipt.Detail) == "" {
		return fmt.Errorf("setup baseline pending audit receipt is incomplete")
	}
	return nil
}

func setupBaselineAuditRecorded(store *Store, repository string, receipt setupBaselineAuditReceipt) (bool, error) {
	if info, statErr := os.Lstat(store.auditPath); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("Hive audit path is linked or non-regular")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	file, err := os.Open(store.auditPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var entry AuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return false, fmt.Errorf("decode durable audit receipt: %w", err)
		}
		if entry.Repository == repository && entry.Action == receipt.Action && entry.Allowed == receipt.Allowed && entry.Detail == setupBaselineAuditReceiptDetail(receipt) {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func flushSetupBaselineAudit(store *Store, intent SetupBaselineIntent) (SetupBaselineIntent, error) {
	if intent.PendingAudit == nil {
		return intent, nil
	}
	recorded, err := setupBaselineAuditRecorded(store, intent.Repository, *intent.PendingAudit)
	if err != nil {
		return intent, err
	}
	if !recorded {
		if err := store.AuditStrict(AuditEntry{Action: intent.PendingAudit.Action, Allowed: intent.PendingAudit.Allowed, Repository: intent.Repository, Detail: setupBaselineAuditReceiptDetail(*intent.PendingAudit)}); err != nil {
			return intent, err
		}
	}
	intent.PendingAudit = nil
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		return intent, err
	}
	return intent, nil
}

func saveSetupBaselineTransition(store *Store, intent SetupBaselineIntent, action string, allowed bool, detail string) (SetupBaselineIntent, error) {
	receipt, err := newSetupBaselineAuditReceipt(action, allowed, detail)
	if err != nil {
		return intent, err
	}
	intent.PendingAudit = &receipt
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		return intent, err
	}
	return flushSetupBaselineAudit(store, intent)
}

func flushSetupBaselineRebindAudit(store *Store, intent SetupBaselineRebindIntent) (SetupBaselineRebindIntent, error) {
	if intent.PendingAudit == nil {
		return intent, nil
	}
	recorded, err := setupBaselineAuditRecorded(store, intent.Repository, *intent.PendingAudit)
	if err != nil {
		return intent, err
	}
	if !recorded {
		if err := store.AuditStrict(AuditEntry{Action: intent.PendingAudit.Action, Allowed: intent.PendingAudit.Allowed, Repository: intent.Repository, Detail: setupBaselineAuditReceiptDetail(*intent.PendingAudit)}); err != nil {
			return intent, err
		}
	}
	intent.PendingAudit = nil
	if err := store.SaveSetupBaselineRebindIntent(intent); err != nil {
		return intent, err
	}
	return intent, nil
}

func saveSetupBaselineRebindTransition(store *Store, intent SetupBaselineRebindIntent, action string, allowed bool, detail string) (SetupBaselineRebindIntent, error) {
	receipt, err := newSetupBaselineAuditReceipt(action, allowed, detail)
	if err != nil {
		return intent, err
	}
	intent.PendingAudit = &receipt
	if err := store.SaveSetupBaselineRebindIntent(intent); err != nil {
		return intent, err
	}
	return flushSetupBaselineRebindAudit(store, intent)
}

func newSetupBaselineAuditReceipt(action string, allowed bool, detail string) (setupBaselineAuditReceipt, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return setupBaselineAuditReceipt{}, err
	}
	return setupBaselineAuditReceipt{ID: fmt.Sprintf("%x", value), Action: action, Allowed: allowed, Detail: detail}, nil
}

func setupBaselineAuditReceiptDetail(receipt setupBaselineAuditReceipt) string {
	return receipt.Detail + " audit_receipt=" + receipt.ID
}
