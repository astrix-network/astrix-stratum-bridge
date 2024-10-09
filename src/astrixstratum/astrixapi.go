package astrixstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/astrix-network/astrix-stratum-bridge/src/gostratum"
	"github.com/astrix-network/astrixd/app/appmessage"
	"github.com/astrix-network/astrixd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type AstrixApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	astrixd      *rpcclient.RPCClient
	connected     bool
}

func NewAstrixApi(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*AstrixApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &AstrixApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "astrixapi:"+address)),
		astrixd:      client,
		connected:     true,
	}, nil
}

func (ks *AstrixApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *AstrixApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.astrixd.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from astrix, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.astrixd.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from astrix, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *AstrixApi) reconnect() error {
	if ks.astrixd != nil {
		return ks.astrixd.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.astrixd = client
	return nil
}

func (s *AstrixApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking astrixd sync state")
	}
	for {
		clientInfo, err := s.astrixd.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from astrixd @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("Astrix is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("astrixd synced, starting server")
	}
	return nil
}

func (s *AstrixApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.astrixd.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from astrix")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking astrixd sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to astrixd, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *AstrixApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.astrixd.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via astrix-network/astrix-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from astrix")
	}
	return template, nil
}
