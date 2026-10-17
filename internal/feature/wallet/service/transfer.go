package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"wallet-transfer-assignment/internal/feature/wallet/domain"
	"wallet-transfer-assignment/internal/feature/wallet/repository"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

var (
	ErrInvalidTransferRequest = errors.New("invalid transfer request")

	// ErrRequestInProgress means the idempotency key is claimed but its original
	// request has not stored a response yet; the client should retry later.
	ErrRequestInProgress = errors.New("request with this idempotency key is still in progress")
)

// canonicalJSON re-serialises a stored JSONB body into the exact compact form
// of a first response. Postgres JSONB adds whitespace (and may reorder keys),
// so without this a replay would not be byte-identical to the original.
func canonicalJSON(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	out, err := json.Marshal(v) // map keys are emitted sorted, like fiber.Map
	if err != nil {
		return body
	}
	return out
}

type CreateTransferRequest struct {
	IdempotencyKey string
	FromWalletID   int64
	ToWalletID     int64
	Amount         int64
}

type TransferResult struct {
	StatusCode int
	Body       []byte
}

type TransferService interface {
	CreateTransfer(
		ctx context.Context,
		req CreateTransferRequest,
	) (*TransferResult, error)
}

type transferService struct {
	walletRepo      repository.WalletRepository
	transferRepo    repository.TransferRepository
	ledgerRepo      repository.LedgerRepository
	idempotencyRepo repository.IdempotencyRepository
	txManager       repository.TransactionManager
}

// Constructor
func NewTransferService(
	walletRepo repository.WalletRepository,
	transferRepo repository.TransferRepository,
	ledgerRepo repository.LedgerRepository,
	idempotencyRepo repository.IdempotencyRepository,
	txManager repository.TransactionManager,
) TransferService {
	return &transferService{
		walletRepo:      walletRepo,
		transferRepo:    transferRepo,
		ledgerRepo:      ledgerRepo,
		idempotencyRepo: idempotencyRepo,
		txManager:       txManager,
	}
}

// CreateTransfer handles wallet-to-wallet transfers.
func (s *transferService) CreateTransfer(ctx context.Context, req CreateTransferRequest) (*TransferResult, error) {
	// Step 1: Validate request
	transfer := domain.Transfer{
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
		Status:         domain.TransferPending,
	}
	if err := transfer.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTransferRequest, err)
	}

	// Step 2: Calculate request fingerprint
	requestHash, err := domain.TransferFingerprint(transfer.FromWalletID, transfer.ToWalletID, transfer.Amount)
	if err != nil {
		return nil, fmt.Errorf("generate request fingerprint: %w", err)
	}

	// Step 3: Start database transaction

	var result *TransferResult

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Reset per-attempt state so retries start from a clean transfer.
		transfer.ID = 0
		transfer.Status = domain.TransferPending
		transfer.FailureReason = nil

		err = s.txManager.WithinTransaction(ctx, func(tx *gorm.DB) error {
			// Claim idempotency key
			record := domain.IdempotencyRecord{
				IdempotencyKey: transfer.IdempotencyKey,
				RequestHash:    requestHash,
			}

			created, err := s.idempotencyRepo.Create(ctx, tx, &record)
			if err != nil {
				return err
			}

			// Key already exists
			if !created {
				existing, err := s.idempotencyRepo.GetByKey(
					ctx,
					tx,
					transfer.IdempotencyKey,
				)
				if err != nil {
					return err
				}

				// Same key but different request
				if err := existing.MatchesRequest(requestHash); err != nil {
					return err
				}

				// Previous response must exist
				if existing.ResponseStatus == nil || existing.ResponseBody == nil {
					return ErrRequestInProgress
				}

				// Same key + same request: return saved response
				result = &TransferResult{
					StatusCode: *existing.ResponseStatus,
					Body:       canonicalJSON(existing.ResponseBody),
				}

				return nil
			}

			// Lock wallets BEFORE inserting the transfer: the FK on transfers takes
			// FOR KEY SHARE on both wallets, which deadlocks with concurrent FOR UPDATE.

			// Step 7: Lock wallets in ascending ID order
			firstID := transfer.FromWalletID
			secondID := transfer.ToWalletID
			// to aviod deadlocak
			if firstID > secondID {
				firstID, secondID = secondID, firstID
			}
			// Lock first wallet
			sender, err := s.walletRepo.GetByIDForUpdate(ctx, tx, firstID)
			if err != nil {
				return fmt.Errorf("lock first wallet: %w", err)
			}
			// Lock second wallet
			receiver, err := s.walletRepo.GetByIDForUpdate(ctx, tx, secondID)
			if err != nil {
				return fmt.Errorf("lock second wallet: %w", err)
			}
			// Assign correct sender and receiver
			if transfer.FromWalletID == secondID {
				sender, receiver = receiver, sender
			}

			created, err = s.transferRepo.Create(ctx, tx, &transfer)
			if err != nil {
				return fmt.Errorf("create transfer: %w", err)
			}

			if !created {
				return errors.New("transfer already exists")
			}

			// Step 8: Check sender balance
			// Step 9: Handle insufficient balance
			if err := sender.CanDebit(transfer.Amount); err != nil {
				if !errors.Is(err, domain.ErrInsufficientFunds) {
					return err
				}
				// Mark transfer as FAILED
				if err := transfer.ChangeStatus(domain.TransferFailed); err != nil {
					return err
				}

				reason := "INSUFFICIENT_FUNDS"
				transfer.FailureReason = &reason

				// Update transfer status in database
				if err := s.transferRepo.UpdateStatus(ctx, tx, &transfer); err != nil {
					return err
				}

				// Prepare HTTP response
				responseBody := []byte(`{"error":"insufficient funds"}`)
				statusCode := fiber.StatusConflict // 409

				// Save response for idempotency replay
				record.TransferID = &transfer.ID
				record.ResponseStatus = &statusCode
				record.ResponseBody = responseBody

				if err := s.idempotencyRepo.SaveResponse(ctx, tx, &record); err != nil {
					return err
				}
				result = &TransferResult{
					StatusCode: statusCode,
					Body:       responseBody,
				}
				return nil // COMMIT
			}

			// Step 10: Calculate new balances
			// Prevent receiver balance overflow
			if receiver.Balance > math.MaxInt64-transfer.Amount {
				return errors.New("receiver balance overflow")
			}

			if err = s.walletRepo.UpdateBalance(ctx, tx, sender.ID, -transfer.Amount); err != nil {
				return fmt.Errorf("debit sender: %w", err)
			}
			// Update receiver balance in database
			if err = s.walletRepo.UpdateBalance(ctx, tx, receiver.ID, transfer.Amount); err != nil {
				return fmt.Errorf("credit receiver: %w", err)
			}

			// Step 11: Create ledger entries
			entries, err := domain.NewLedgerEntries(
				transfer.ID,
				sender.ID,
				receiver.ID,
				transfer.Amount,
			)
			if err != nil {
				return fmt.Errorf("create ledger entries: %w", err)
			}

			err = s.ledgerRepo.CreateEntries(ctx, tx, entries)
			if err != nil {
				return fmt.Errorf("save ledger entries: %w", err)
			}

			// Step 12: Mark transfer as PROCESSED
			if err := transfer.ChangeStatus(domain.TransferProcessed); err != nil {
				return fmt.Errorf("change transfer status: %w", err)
			}

			err = s.transferRepo.UpdateStatus(ctx, tx, &transfer)
			if err != nil {
				return fmt.Errorf("update transfer status: %w", err)
			}

			// Step 13: Prepare success response
			responseBody, err := json.Marshal(fiber.Map{
				"transferId": transfer.ID,
				"status":     transfer.Status,
			})
			if err != nil {
				return fmt.Errorf("marshal transfer response: %w", err)
			}
			statusCode := fiber.StatusCreated // 201

			record.TransferID = &transfer.ID
			record.ResponseStatus = &statusCode
			record.ResponseBody = responseBody

			if err := s.idempotencyRepo.SaveResponse(ctx, tx, &record); err != nil {
				return fmt.Errorf("save idempotency response: %w", err)
			}

			result = &TransferResult{
				StatusCode: statusCode,
				Body:       responseBody,
			}
			return nil // COMMIT transaction
		})

		if err == nil {
			return result, nil
		}
		// 2. Commit result uncertain - don't blindly retry
		if errors.Is(err, repository.ErrCommitUncertain) {
			savedResult, lookupErr := s.getSavedResponse(
				ctx,
				transfer.IdempotencyKey,
				requestHash,
			)
			if lookupErr == nil {
				return savedResult, nil
			}
			// Outcome could not be confirmed
			return nil, err
		}

		if !isRetryableDBError(err) {
			return nil, err
		}
		if attempt == maxAttempts {
			return nil, err
		}
		// Reset result before retry
		result = nil

		// Exponential backoff
		delay := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, errors.New("transfer retries exhausted")
}

func (s *transferService) getSavedResponse(ctx context.Context, key, requestHash string) (*TransferResult, error) {
	var result *TransferResult
	err := s.txManager.WithinTransaction(ctx, func(tx *gorm.DB) error {
		record, err := s.idempotencyRepo.GetByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if err := record.MatchesRequest(requestHash); err != nil {
			return err
		}
		if record.ResponseStatus == nil || record.ResponseBody == nil {
			return ErrRequestInProgress
		}
		result = &TransferResult{
			StatusCode: *record.ResponseStatus,
			Body:       canonicalJSON(record.ResponseBody),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}
