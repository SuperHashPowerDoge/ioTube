// Copyright (c) 2020 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package witness

import (
	"context"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/iotexproject/ioTube/witness-service/grpc/services"
	"github.com/iotexproject/ioTube/witness-service/grpc/types"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

type (
	tokenCashierBase struct {
		id                     string
		recorder               *Recorder
		relayerURL             string
		validatorContractAddr  common.Address
		startBlockHeight       uint64
		lastProcessBlockHeight uint64
		lastPatrolBlockHeight  uint64
		lastPullTimestamp      time.Time
		calcEndHeight          calcEndHeightFunc
		pullTransfers          pullTransfersFunc
		hasEnoughBalance       hasEnoughBalanceFunc
		start                  startStopFunc
		stop                   startStopFunc
	}
	calcEndHeightFunc    func(startHeight uint64, count uint16) (uint64, error)
	pullTransfersFunc    func(startHeight uint64, endHeight uint64) ([]*Transfer, error)
	hasEnoughBalanceFunc func(token common.Address, amount *big.Int) bool
	startStopFunc        func(context.Context) error
)

func newTokenCashierBase(
	id string,
	recorder *Recorder,
	relayerURL string,
	validatorContractAddr common.Address,
	startBlockHeight uint64,
	calcEndHeight calcEndHeightFunc,
	pullTransfers pullTransfersFunc,
	hasEnoughBalance hasEnoughBalanceFunc,
	start startStopFunc,
	stop startStopFunc,
) TokenCashier {
	return &tokenCashierBase{
		id:                     id,
		recorder:               recorder,
		relayerURL:             relayerURL,
		startBlockHeight:       startBlockHeight,
		lastProcessBlockHeight: startBlockHeight,
		validatorContractAddr:  validatorContractAddr,
		calcEndHeight:          calcEndHeight,
		pullTransfers:          pullTransfers,
		hasEnoughBalance:       hasEnoughBalance,
		lastPullTimestamp:      time.Now(),
		start:                  start,
		stop:                   stop,
	}
}

func (tc *tokenCashierBase) Start(ctx context.Context) error {
	if err := tc.recorder.Start(ctx); err != nil {
		return err
	}
	return tc.start(ctx)
}

func (tc *tokenCashierBase) Stop(ctx context.Context) error {
	if err := tc.stop(ctx); err != nil {
		return err
	}
	return tc.recorder.Stop(ctx)
}

func (tc *tokenCashierBase) GetRecorder() *Recorder {
	return tc.recorder
}

func (tc *tokenCashierBase) PullTransfersByHeight(height uint64) error {
	transfers, err := tc.pullTransfers(height, height)
	if err != nil {
		return errors.Wrapf(err, "failed to pull transfers for %d", height)
	}
	for _, transfer := range transfers {
		if err := tc.recorder.UpsertTransfer(transfer); err != nil {
			return errors.Wrap(err, "failed to add transfer")
		}
	}
	return nil
}

func (tc *tokenCashierBase) PullTransfers(count uint16) error {
	startHeight, err := tc.recorder.TipHeight(tc.id)
	if err != nil {
		return err
	}
	if startHeight < tc.lastProcessBlockHeight {
		startHeight = tc.lastProcessBlockHeight
	}
	if count == 0 {
		count = 1
	}
	patrolSize := uint64(count) * 3
	if tc.lastPatrolBlockHeight == 0 && startHeight > patrolSize {
		tc.lastPatrolBlockHeight = startHeight - patrolSize
		if tc.lastPatrolBlockHeight < tc.startBlockHeight {
			tc.lastPatrolBlockHeight = tc.startBlockHeight
		}
	}
	startHeight = startHeight + 1
	endHeight, err := tc.calcEndHeight(startHeight, count)
	if err != nil {
		if tc.lastPullTimestamp.Add(3 * time.Minute).After(time.Now()) {
			log.Printf("failed to get end height with start height %d, count %d: %+v\n", startHeight, endHeight, err)
			return nil
		}
		return errors.Wrapf(err, "failed to get end height with start height %d, count %d", startHeight, count)
	}
	var transfers []*Transfer
	tc.lastPullTimestamp = time.Now()
	if startHeight > tc.lastPatrolBlockHeight+patrolSize {
		log.Printf("fetching events from block %d to %d for %s with patrol\n", startHeight, endHeight, tc.id)
		transfers, err = tc.pullTransfers(tc.lastPatrolBlockHeight, endHeight)
		tc.lastPatrolBlockHeight = startHeight
	} else {
		log.Printf("fetching events from block %d to %d for %s\n", startHeight, endHeight, tc.id)
		transfers, err = tc.pullTransfers(startHeight, endHeight)
	}
	if err != nil {
		return errors.Wrapf(err, "failed to pull transfers from %d to %d", startHeight, endHeight)
	}
	for _, transfer := range transfers {
		if err := tc.recorder.AddTransfer(transfer); err != nil {
			return errors.Wrap(err, "failed to add transfer")
		}
	}
	tc.lastProcessBlockHeight = endHeight

	return tc.recorder.UpdateSyncHeight(tc.id, endHeight)
}

func (tc *tokenCashierBase) SubmitTransfers(sign func(*Transfer, common.Address) (common.Hash, common.Address, []byte, error)) error {
	transfersToSubmit, err := tc.recorder.TransfersToSubmit()
	if err != nil {
		return err
	}
	conn, err := grpc.Dial(tc.relayerURL, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()
	relayer := services.NewRelayServiceClient(conn)
	for _, transfer := range transfersToSubmit {
		if !tc.hasEnoughBalance(transfer.token, transfer.amount) {
			return errors.Errorf("not enough balance for token %s", transfer.token)
		}
		id, witness, signature, err := sign(transfer, tc.validatorContractAddr)
		if err != nil {
			return err
		}
		transfer.id = id
		response, err := relayer.Submit(
			context.Background(),
			&types.Witness{
				Transfer: &types.Transfer{
					Cashier:   transfer.cashier.Bytes(),
					Token:     transfer.coToken.Bytes(),
					Index:     int64(transfer.index),
					Sender:    transfer.sender.Bytes(),
					Recipient: transfer.recipient.Bytes(),
					Amount:    transfer.amount.String(),
					Fee:       transfer.fee.String(),
				},
				Address:   witness.Bytes(),
				Signature: signature,
			},
		)
		if err != nil {
			return err
		}
		if response.Success {
			if err := tc.recorder.ConfirmTransfer(transfer); err != nil {
				return err
			}
		} else {
			log.Printf("something went wrong when submitting transfer (%s, %s, %d) for %s\n", transfer.cashier, transfer.token, transfer.index, tc.id)
		}
	}
	return nil
}

func (tc *tokenCashierBase) CheckTransfers() error {
	transfersToSettle, err := tc.recorder.TransfersToSettle()
	if err != nil {
		return err
	}
	conn, err := grpc.Dial(tc.relayerURL, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()
	relayer := services.NewRelayServiceClient(conn)

	for _, transfer := range transfersToSettle {
		response, err := relayer.Check(
			context.Background(),
			&services.CheckRequest{Id: transfer.id.Bytes()},
		)
		if err != nil {
			return err
		}
		if response.Status == services.Status_SETTLED {
			if err := tc.recorder.SettleTransfer(transfer); err != nil {
				return err
			}
		}
	}
	return nil
}
