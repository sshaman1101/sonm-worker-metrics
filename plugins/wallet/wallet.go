package wallet

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sonm-io/core/blockchain"
	sonm "github.com/sonm-io/core/proto"
	"github.com/sonm-io/core/util"
	"github.com/sonm-io/monitoring/influx"
	"github.com/sonm-io/monitoring/plugins"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	Addresses []common.Address `yaml:"addresses"`
}

type walletRow struct {
	Addr    common.Address
	Balance float64
	Deals   uint64
	Orders  uint64
}

type walletPlugin struct {
	cfg *Config
	log *zap.Logger
	bta blockchain.TokenAPI
	dwh sonm.DWHClient
	inf *influx.Influx
}

func NewWalletPlugin(cfg *Config, log *zap.Logger, inf *influx.Influx, bta blockchain.TokenAPI, dwh sonm.DWHClient) plugins.Plugin {
	return &walletPlugin{
		cfg: cfg,
		log: log.Named("wallet"),
		bta: bta,
		dwh: dwh,
		inf: inf,
	}
}

func (m *walletPlugin) Run(ctx context.Context) {
	tk := util.NewImmediateTicker(60 * time.Second)
	defer tk.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log.Warn("stopping by the context", zap.Error(ctx.Err()))
			return
		case <-tk.C:
			m.once(ctx)
		}
	}
}

func (m *walletPlugin) once(ctx context.Context) {
	for i := range m.cfg.Addresses {
		row, err := m.collect(ctx, m.cfg.Addresses[i])
		if err == nil {
			m.log.Debug("row collection done", zap.Any("data", *row))
			err = m.inf.WriteRaw(
				"wallets",
				map[string]string{"addr": row.Addr.String()},
				map[string]interface{}{
					"balance": row.Balance,
					"deals":   int64(row.Deals),
					"orders":  int64(row.Orders),
				},
			)
			if err != nil {
				m.log.Warn("failed to write wallet data info influxdb", zap.Error(err))
			}
		}
	}
}

func (m *walletPlugin) collect(ctx context.Context, addr common.Address) (*walletRow, error) {
	row := &walletRow{Addr: addr}
	wg, ctx := errgroup.WithContext(ctx)

	// token balance
	wg.Go(func() error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		bal, err := m.bta.BalanceOf(ctx, addr)
		if err != nil {
			m.log.Warn("failed to get balance", zap.Error(err), zap.Stringer("addr", addr))
			return err
		}

		bf := big.NewFloat(0).SetInt(bal.SNM)
		f64, _ := big.NewFloat(0).Quo(bf, big.NewFloat(1e18)).Float64()
		row.Balance = f64

		return nil
	})

	// deals@DWH
	wg.Go(func() error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		deals, err := m.dwh.GetDeals(ctx, &sonm.DealsRequest{
			Status:    sonm.DealStatus_DEAL_ACCEPTED,
			Limit:     500,
			WithCount: true,
			AnyUserID: sonm.NewEthAddress(addr),
		})
		if err != nil {
			m.log.Warn("failed to get deals list", zap.Error(err), zap.Stringer("addr", addr))
			return err
		}

		row.Deals = deals.GetCount()
		return nil
	})

	// orders@DWH
	wg.Go(func() error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		orders, err := m.dwh.GetOrders(ctx, &sonm.OrdersRequest{
			Status:    sonm.OrderStatus_ORDER_ACTIVE,
			Type:      sonm.OrderType_ANY,
			Limit:     500,
			WithCount: true,
			AuthorID:  sonm.NewEthAddress(addr),
		})
		if err != nil {
			m.log.Warn("failed to get orders list", zap.Error(err), zap.Stringer("addr", addr))
			return err
		}

		row.Orders = orders.GetCount()
		return nil
	})

	if err := wg.Wait(); err != nil {
		return nil, err
	}

	return row, nil
}
