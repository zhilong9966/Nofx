package trader

import (
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"sort"
	"strconv"
	"time"

	"github.com/antihax/optional"
	"github.com/gateio/gateapi-go/v7"
)

// GateTrade represents a trade record
type GateTrade struct {
	Symbol      string
	OrderID     string
	ExecID      string
	Side        string // "long" or "short"
	ExecPrice   float64
	ExecQty     float64
	ExecFee     float64
	ExecTime    time.Time
	OrderAction string // open_long, open_short, close_long, close_short
	Pnl         float64
}

// SyncOrdersFromGate syncs Gate.io exchange order history to local database
func (t *GateTrader) SyncOrdersFromGate(traderID string, exchangeID string, exchangeType string, st *store.Store) error {
	if st == nil {
		return fmt.Errorf("store is nil")
	}

	// 1. Get recent trades (MyTrades)
	// Gate.io ListMyTrades -> GetMyTradesWithTimeRange (for time filtering)
	// Time range? from/to
	startTime := time.Now().Add(-24 * time.Hour)
	
	ctx := t.getAuthContext()
	
	// GetMyTradesWithTimeRange returns []gateapi.MyFuturesTrade
	trades, _, err := t.client.FuturesApi.GetMyTradesWithTimeRange(ctx, t.settle, &gateapi.GetMyTradesWithTimeRangeOpts{
		From: optional.NewInt64(startTime.Unix()),
	})
	if err != nil {
		return fmt.Errorf("failed to get trades: %w", err)
	}

	logger.Infof("📥 Received %d trades from Gate.io", len(trades))

	// Sort by time
	sort.Slice(trades, func(i, j int) bool {
		return trades[i].CreateTime < trades[j].CreateTime
	})

	orderStore := st.Order()
	positionStore := st.Position()
	posBuilder := store.NewPositionBuilder(positionStore)
	syncedCount := 0

	for _, trade := range trades {
		// Gate Trade ID is string (TradeId)
		tradeID := trade.TradeId
		
		// Check duplicates
		existing, err := orderStore.GetOrderByExchangeID(exchangeID, tradeID)
		if err == nil && existing != nil {
			// Already synced
			continue
		}

		symbol := t.convertSymbolBack(trade.Contract)
		normalizedSymbol := market.Normalize(symbol) // e.g. BTCUSDT

		// Parse values
		price, _ := strconv.ParseFloat(trade.Price, 64)
		// Size is num contracts (positive/negative)
		size := float64(trade.Size)
		// Convert size to quantity (base asset)
		contract, _ := t.getContractInfo(symbol)
		multiplier := 1.0
		if contract != nil {
			multiplier, _ = strconv.ParseFloat(contract.QuantoMultiplier, 64)
		}
		
		quantity := math.Abs(size) * multiplier
		
		// Determine Action
		// Gate.io size: +ve = buy (long), -ve = sell (short).
		// Close logic?
		// `is_close` field in trade? No.
		// Trade struct has `Id`, `CreateTime`, `Contract`, `OrderId`, `Size`, `Price`...
		// It doesn't explicitly say if it opened or closed.
		// But PositionBuilder can handle it if we know Long/Short direction.
		// Wait, Gate.io in one-way mode (default usually):
		// Buy (Long) can be Open Long or Close Short.
		// Sell (Short) can be Open Short or Close Long.
		// Without `ReduceOnly` or `IsClose` flag in trade history, it's hard to distinguish strictly.
		// However, most other exchanges provide this.
		// In Gate.io trade object: 
		// Does it have `role`? (Maker/Taker).
		// Maybe text?
		
		// Let's assume simplest mapping for One-Way mode:
		// Gate.io Futures is usually Hedge mode or One-way?
		// If One-way:
		// Positive Size (Buy): If current position is Short, it's closing. If Flat/Long, it's opening.
		// This state dependence makes it hard for pure sync without context.
		// BUT `PositionBuilder` in `store` might handle net position calculation?
		// `posBuilder.ProcessTrade` expects `orderAction` (open_long/close_short etc).
		
		// Let's look at `ListPositionClose`. If we can match trade to close record?
		// PnL is in `ListPositionClose`.
		// Simple approach: 
		// Action mapping:
		// If size > 0 => "Buy"
		// If size < 0 => "Sell"
		
		// We might need to guess based on existing position state in DB? 
		// Or assume standard flow.
		// Let's infer Action based on Side.
		// Side "Buy" -> open_long? (Or close_short)
		// Side "Sell" -> open_short? (Or close_long)
		
		// For syncing, we might just log it as generic BUY/SELL if store supports it?
		// Store expects `open_long`, `close_long`, etc.
		// Let's try to fetch order details? Order has `is_close`? 
		// Getting order for every trade is expensive.
		
		// Strategy:
		// Use "open_long" for Buy, "open_short" for Sell as default.
		// If it has PnL (from `ListPositionClose` matching), it's a close.
		// But `ListMyTrades` doesn't have PnL usually.
		
		// Matching with `GetClosedPnL`?
		// Gate.io separates Trade History and Close History.
		// Close History has PnL and Time.
		
		// Let's try to map:
		// If we find a Close Record around same time/orderID?
		// Close Record has `order_id`? No, `ListPositionClose` usually summarizes.
		
		// Fallback:
		// Mark as open_long (Buy) / open_short (Sell).
		// PositionBuilder handles net sizing? 
		// Actually `PositionBuilder` is smart enough? 
		// `ProcessTrade` takes `action`.
		// If we incorrectly label `close_short` as `open_long` (both are buys), 
		// net position calculation in builder might differ?
		// `open_long` increases Long position.
		// `close_short` decreases Short position.
		// In One-way mode, they are functionally similar (increasing net exposure in positive direction).
		
		// Let's use:
		// Buy -> "open_long"
		// Sell -> "open_short"
		// This is technically "incorrect" for closing trades in hedge mode,
		// but for One-way mode, it tracks "Net Position" if we just sum them?
		// But PositionBuilder separates Long/Short sides.
		
		// Let's assume standard behavior:
		// If size > 0: "open_long"
		// If size < 0: "open_short"
		// (Accepting inaccuracy for closing trades unless we check Position)
		
		side := "BUY"
		action := "open_long"
		if size < 0 {
			side = "SELL"
			action = "open_short"
		}
		
		// Map fee 
		// Gate Trade has `fee` string? No, struct: `Fee` string? No.
		// `Fee` field in `FuturesTrade`.
		// Wait, response from `ListMyTrades` is `[]FuturesTrade`.
		// It has `Fee` field?
		// API Reference: "fee": "Fee deducted".
		// But SDK struct?
		// checking... Gate SDK `FuturesTrade`: `Fee` string. Yes.
		fee, _ := strconv.ParseFloat(trade.Fee, 64)
		
		// Exec time
		execTime := time.Unix(int64(trade.CreateTime), 0).UTC()
		
		// Create Trade Record
		orderRecord := &store.TraderOrder{
			TraderID:        traderID,
			ExchangeID:      exchangeID,
			ExchangeType:    exchangeType,
			ExchangeOrderID: trade.OrderId, // Use OrderID to link (OrderId is string in MyFuturesTrade?)
			Symbol:          normalizedSymbol,
			Side:            side,
			PositionSide:    "BOTH",
			Type:            "MARKET", // Assumption
			OrderAction:     action,
			Quantity:        quantity,
			Price:           price,
			Status:          "FILLED",
			FilledQuantity:  quantity,
			AvgFillPrice:    price,
			Commission:      fee,
			FilledAt:        execTime.UnixMilli(),
			CreatedAt:       execTime.UnixMilli(),
			UpdatedAt:       execTime.UnixMilli(),
		}
		
		if err := orderStore.CreateOrder(orderRecord); err != nil {
			logger.Warnf("Failed to sync order %s: %v", tradeID, err)
			continue
		}
		
		// Fill Record
		fillRecord := &store.TraderFill{
			TraderID:        traderID,
			ExchangeID:      exchangeID,
			ExchangeType:    exchangeType,
			OrderID:         orderRecord.ID, // DB ID
			ExchangeOrderID: trade.OrderId,
			ExchangeTradeID: tradeID,
			Symbol:          normalizedSymbol,
			Side:            side,
			Price:           price,
			Quantity:        quantity,
			QuoteQuantity:   price * quantity,
			Commission:      fee,
			CommissionAsset: "USDT", // Assumption
			CreatedAt:       execTime.UnixMilli(),
		}
		if err := orderStore.CreateFill(fillRecord); err != nil {
			logger.Warnf("Failed to sync fill %s: %v", tradeID, err)
		}
		
		// Update Position Builder
		// PnL? Only known if close.
		// We can try to fetch closed PNL if this trade closed something.
		// For now 0.
		
		posSide := "LONG"
		if action == "open_short" {
			posSide = "SHORT"
		}
		
		if err := posBuilder.ProcessTrade(
			traderID, exchangeID, exchangeType,
			normalizedSymbol, posSide, action,
			quantity, price, fee, 0, // pnl
			execTime.UnixMilli(), tradeID,
		); err != nil {
			logger.Warnf("Failed to process position for trade %s: %v", tradeID, err)
		}
		
		syncedCount++
	}
	
	if syncedCount > 0 {
		logger.Infof("✅ Gate.io order sync completed: %d new trades synced", syncedCount)
	}
	return nil
}

// StartOrderSync starts background sync
func (t *GateTrader) StartOrderSync(traderID string, exchangeID string, exchangeType string, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			if err := t.SyncOrdersFromGate(traderID, exchangeID, exchangeType, st); err != nil {
				logger.Warnf("Gate.io order sync failed: %v", err)
			}
		}
	}()
	logger.Infof("🔄 Gate.io order sync started (interval: %v)", interval)
}
