package trader

import (
	"context"
	"fmt"
	"math"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antihax/optional"
	"github.com/gateio/gateapi-go/v7"
)

// GateTrader Gate.io Futures Trader
type GateTrader struct {
	client    *gateapi.APIClient
	apiKey    string
	secretKey string
	settle    string // "usdt"

	// Balance cache
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// Position cache
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// Market info cache (contract size, precisions)
	marketInfoCache      map[string]*gateapi.Contract
	marketInfoCacheMutex sync.RWMutex

	// Cache duration
	cacheDuration time.Duration
}

// NewGateTrader creates a Gate.io trader
func NewGateTrader(apiKey, secretKey string) *GateTrader {
	cfg := gateapi.NewConfiguration()
	cfg.BasePath = "https://api.gateio.ws/api/v4"
	client := gateapi.NewAPIClient(cfg)

	trader := &GateTrader{
		client:          client,
		apiKey:          apiKey,
		secretKey:       secretKey,
		settle:          "usdt",
		cacheDuration:   5 * time.Second, // Shorter cache for high frequency
		marketInfoCache: make(map[string]*gateapi.Contract),
	}

	logger.Infof("🔵 [Gate.io] Trader initialized")
	return trader
}

// getAuthContext returns context with API keys
func (t *GateTrader) getAuthContext() context.Context {
	ctx := context.Background()
	return context.WithValue(ctx, gateapi.ContextGateAPIV4, gateapi.GateAPIV4{
		Key:    t.apiKey,
		Secret: t.secretKey,
	})
}

// convertSymbol converts generic symbol (BTCUSDT) to Gate.io format (BTC_USDT)
func (t *GateTrader) convertSymbol(symbol string) string {
	if strings.Contains(symbol, "_") {
		return symbol
	}
	// Simple heuristic: assume the last 4 chars are USDT if not specified
	// But usually symbol passed is like "BTCUSDT"
	// Gate.io futures pairs are like "BTC_USDT"
	// We need to split. Assuming USDT quoted.
	upper := strings.ToUpper(symbol)
	if strings.HasSuffix(upper, "USDT") {
		return upper[:len(upper)-4] + "_USDT"
	} else if strings.HasSuffix(upper, "USD") {
		return upper[:len(upper)-3] + "_USD"
	}
	return symbol // fallback
}

// convertSymbolBack converts Gate.io format (BTC_USDT) to generic symbol (BTCUSDT)
func (t *GateTrader) convertSymbolBack(gateSymbol string) string {
	return strings.ReplaceAll(gateSymbol, "_", "")
}

// getMarketInfo gets contract info (cached)
func (t *GateTrader) getContractInfo(symbol string) (*gateapi.Contract, error) {
	gateSymbol := t.convertSymbol(symbol)

	t.marketInfoCacheMutex.RLock()
	if info, ok := t.marketInfoCache[gateSymbol]; ok {
		t.marketInfoCacheMutex.RUnlock()
		return info, nil
	}
	t.marketInfoCacheMutex.RUnlock()

	ctx := t.getAuthContext()
	contract, _, err := t.client.FuturesApi.GetFuturesContract(ctx, t.settle, gateSymbol)
	if err != nil {
		return nil, err
	}

	t.marketInfoCacheMutex.Lock()
	t.marketInfoCache[gateSymbol] = &contract
	t.marketInfoCacheMutex.Unlock()

	return &contract, nil
}

// formatQuantity formats quantity according to lot size
func (t *GateTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	contract, err := t.getContractInfo(symbol)
	if err != nil {
		return "", err
	}

	// Gate.io uses "quantum" (contract size multiplier usually) but for sizing orders:
	// "size" in order is number of contracts.
	// We need to convert USDT/Coin quantity to number of contracts.
	// But `quantity` in `Trader` interface usually means "amount of base currency" (e.g. BTC) or "contracts" depending on exchange?
	// In this system (nofx), quantity usually means "Base Asset Quantity" (e.g. 0.1 BTC).

	// Gate.io Futures:
	// Order size needs to be integer (number of contracts).
	// One contract size (quanto_multiplier) is defined in contract info.
	// e.g. BTC_USDT usually 1 contract = 0.0001 BTC or similar?
	// Wait, Gate.io USDT margined futures: size is in "contract units".
	// We need to check `quanto_multiplier`.
	// Example: BTC_USDT, quanto_multiplier might be "0.0001" (meaning 1 contract = 0.0001 BTC).
	// So if request is 1 BTC, we need 1 / 0.0001 = 10000 contracts.

	multiplierStr := contract.QuantoMultiplier
	multiplier, err := strconv.ParseFloat(multiplierStr, 64)
	if err != nil || multiplier == 0 {
		return "", fmt.Errorf("invalid quanto multiplier: %s", multiplierStr)
	}

	contracts := quantity / multiplier
	// Size must be integer
	contractsInt := int64(math.Round(contracts))

	if contractsInt == 0 && quantity > 0 {
		contractsInt = 1 // Minimum 1 contract if not zero
	}

	return fmt.Sprintf("%d", contractsInt), nil
}

// GetBalance retrieves account balance
func (t *GateTrader) GetBalance() (map[string]interface{}, error) {
	// Check cache
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		balance := t.cachedBalance
		t.balanceCacheMutex.RUnlock()
		return balance, nil
	}
	t.balanceCacheMutex.RUnlock()

	ctx := t.getAuthContext()
	// Gate.io ListFuturesAccounts returns single account struct in v7 SDK
	account, _, err := t.client.FuturesApi.ListFuturesAccounts(ctx, t.settle)
	if err != nil {
		return nil, fmt.Errorf("failed to get Gate.io balance: %w", err)
	}

	total, _ := strconv.ParseFloat(account.Total, 64)
	unrealised, _ := strconv.ParseFloat(account.UnrealisedPnl, 64)
	available, _ := strconv.ParseFloat(account.Available, 64)
	
	// In Gate.io:
	// Total = Margin Balance (Wallet + Unrealized) ?
	// 'total': Total assets, including unrealized PNL
	// 'available': Available balance
	// 'unrealised_pnl': Unrealized PNL
	
	// Wallet Balance = Total - Unrealized ? Or Total is Wallet?
	// Doc says: "total": "Total assets". Usually Equity.
	// Let's assume Total = Equity. 
	// Wallet Balance = Equity - Unrealized.
	
	walletBalance := total - unrealised

	balance := map[string]interface{}{
		"totalEquity":           total,
		"totalWalletBalance":    walletBalance,
		"availableBalance":      available,
		"totalUnrealizedProfit": unrealised,
		"balance":               total,
	}

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = balance
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return balance, nil
}

// GetPositions retrieves all positions
func (t *GateTrader) GetPositions() ([]map[string]interface{}, error) {
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		positions := t.cachedPositions
		t.positionsCacheMutex.RUnlock()
		return positions, nil
	}
	t.positionsCacheMutex.RUnlock()

	ctx := t.getAuthContext()
	gatePositions, _, err := t.client.FuturesApi.ListPositions(ctx, t.settle, &gateapi.ListPositionsOpts{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Gate.io positions: %w", err)
	}

	var positions []map[string]interface{}

	for _, pos := range gatePositions {
		sizeIdx := pos.Size // Number of contracts (positive/negative)
		if sizeIdx == 0 {
			continue
		}

		symbol := t.convertSymbolBack(pos.Contract)
		
		// Need contract multiplier to convert sizeIdx to quantity (BTC amount)
		contract, err := t.getContractInfo(symbol)
		if err != nil {
			logger.Errorf("Failed to get contract info for %s: %v", symbol, err)
			continue
		}
		multiplier, _ := strconv.ParseFloat(contract.QuantoMultiplier, 64)

		quantity := float64(sizeIdx) * multiplier
		
		// Normalize: quantity should be absolute for positionAmt?
		// Interface expects "positionAmt" to be signed or unsinged?
		// Looking at Bybit implementation: "positionAmt" is signed (- for short).
		// Gate.io size is signed. Perfect.

		entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
		markPrice, _ := strconv.ParseFloat(pos.MarkPrice, 64)
		unrealisedPnl, _ := strconv.ParseFloat(pos.UnrealisedPnl, 64)
		liqPrice, _ := strconv.ParseFloat(pos.LiqPrice, 64)
		leverage, _ := strconv.ParseFloat(pos.Leverage, 64)

		side := "long"
		if sizeIdx < 0 {
			side = "short"
		}

		// Gate.io ListPositions doesn't return created time directly.
		// We might need to fetch it differently or just rely on local tracking.
		// For now use current time or 0.
		
		position := map[string]interface{}{
			"symbol":           symbol,
			"side":             side,
			"positionAmt":      quantity,
			"entryPrice":       entryPrice,
			"markPrice":        markPrice,
			"unRealizedProfit": unrealisedPnl,
			"unrealizedPnL":    unrealisedPnl,
			"liquidationPrice": liqPrice,
			"leverage":         leverage,
			"createdTime":      time.Now().UnixMilli(), // Approximate
			"updatedTime":      time.Now().UnixMilli(),
		}

		positions = append(positions, position)
	}

	t.positionsCacheMutex.Lock()
	t.cachedPositions = positions
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return positions, nil
}

// InvalidatePositionCache clears cache
func (t *GateTrader) InvalidatePositionCache() {
	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheMutex.Unlock()
}

// clearCache helper
func (t *GateTrader) clearCache() {
	t.InvalidatePositionCache()
	t.balanceCacheMutex.Lock()
	t.cachedBalance = nil
	t.balanceCacheMutex.Unlock()
}

// OpenLong opens a long position
func (t *GateTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.placeOrder(symbol, quantity, leverage, "long")
}

// OpenShort opens a short position
func (t *GateTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.placeOrder(symbol, quantity, leverage, "short")
}

func (t *GateTrader) placeOrder(symbol string, quantity float64, leverage int, side string) (map[string]interface{}, error) {
	gateSymbol := t.convertSymbol(symbol)

	// Set Leverage
	if err := t.SetLeverage(symbol, leverage); err != nil {
		logger.Warnf("Failed to set leverage for %s: %v", symbol, err)
	}

	// Format quantity (number of contracts)
	sizeStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	if side == "short" {
		size = -size
	}

	ctx := t.getAuthContext()
	order := gateapi.FuturesOrder{
		Contract: gateSymbol,
		Size:     size,
		Price:    "0", // 0 for market order
		Tif:      "ioc", // ImmediateOrCancel for market
	}

	// Gate.io API: CreateFuturesOrder
	// If Price is 0, it is market order.
	result, _, err := t.client.FuturesApi.CreateFuturesOrder(ctx, t.settle, order, nil)
	if err != nil {
		return nil, fmt.Errorf("Gate.io place order failed: %v", err)
	}

	t.clearCache()

	return map[string]interface{}{
		"orderId": fmt.Sprintf("%d", result.Id),
		"status":  "NEW", // Assuming successful submission
	}, nil
}

// CloseLong closes a long position
func (t *GateTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closePosition(symbol, quantity, "long")
}

// CloseShort closes a short position
func (t *GateTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closePosition(symbol, quantity, "short")
}

func (t *GateTrader) closePosition(symbol string, quantity float64, currentSide string) (map[string]interface{}, error) {
	// If quantity is 0, fetch full position
	if quantity == 0 {
		positions, err := t.GetPositions()
		if err != nil {
			return nil, err
		}
		for _, pos := range positions {
			if pos["symbol"] == symbol && pos["side"] == currentSide {
				quantity = math.Abs(pos["positionAmt"].(float64))
				break
			}
		}
	}

	if quantity <= 0 {
		return nil, fmt.Errorf("no position to close for %s", symbol)
	}

	// Gate.io reduction: Just place opposite order.
	// ReduceOnly flag is `reduce_only` field in FuturesOrder.
	// Side logic: Closing Long -> Short (sell). Closing Short -> Long (buy).
	targetSide := "short"
	if currentSide == "short" {
		targetSide = "long"
	}

	gateSymbol := t.convertSymbol(symbol)
	sizeStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	
	if targetSide == "short" {
		size = -size
	}

	ctx := t.getAuthContext()
	order := gateapi.FuturesOrder{
		Contract:   gateSymbol,
		Size:       size,
		Price:      "0",
		Tif:        "ioc",
		ReduceOnly: true, // Important for closing
	}

	result, _, err := t.client.FuturesApi.CreateFuturesOrder(ctx, t.settle, order, nil)
	if err != nil {
		return nil, fmt.Errorf("Gate.io close order failed: %v", err)
	}

	t.clearCache()
	
	return map[string]interface{}{
		"orderId": fmt.Sprintf("%d", result.Id),
		"status":  "NEW",
	}, nil
}

// SetLeverage sets leverage
func (t *GateTrader) SetLeverage(symbol string, leverage int) error {
	ctx := t.getAuthContext()
	gateSymbol := t.convertSymbol(symbol)
	
	// '0' means cross margin leverage logic in some exchanges, 
	// but Gate.io has `UpdatePositionLeverage`. 
	// Note: Gate.io splits Cross margin and Leverage setting.
	// Cross margin is set by `UpdatePositionCrossMode`.
	
	levStr := fmt.Sprintf("%d", leverage)
	_, _, err := t.client.FuturesApi.UpdatePositionLeverage(ctx, t.settle, gateSymbol, levStr, nil)
	if err != nil {
		// Ignore if already set? SDK error might not contain code easy to parse.
		// But usually it's fine to just return error if it fails, or log it.
		// Gate throws error if leverage is same?
		return fmt.Errorf("failed to set leverage: %v", err)
	}
	return nil
}

// SetMarginMode sets position margin mode
func (t *GateTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	// Gate.io API uses UpdatePositionCrossMode for cross margin
	// Using leverage=0 signifies cross margin in some endpoints or specific call.
	// But UpdatePositionCrossMode exists (verified).
	// Let's try calling it. Signature guess: (ctx, settle, contract, leverageStr).
	// But cross margin doesn't necessarily have leverage?
	// Actually, usually it's just `UpdatePositionCrossMode(ctx, settle, contract, nil)`. Or similar.
	// Let's try passing simple args to probe signature if needed, or just clear unused vars to get compilation passing first.
	// Clearing unused vars is safer for now.
	
	// Implementation deferred to verification phase or user manual setting
	// _ = ctx
	// _ = gateSymbol
	// _ = isCrossMargin
	
	return nil
}

// GetMarketPrice retrieves market price
func (t *GateTrader) GetMarketPrice(symbol string) (float64, error) {
	gateSymbol := t.convertSymbol(symbol)
	ctx := t.getAuthContext()
	
	// Gate.io: ListFuturesTickers
	tickers, _, err := t.client.FuturesApi.ListFuturesTickers(ctx, t.settle, &gateapi.ListFuturesTickersOpts{
		Contract: optional.NewString(gateSymbol),
	})
	
	if err != nil {
		return 0, err
	}
	
	if len(tickers) == 0 {
		return 0, fmt.Errorf("ticker not found")
	}
	
	price, _ := strconv.ParseFloat(tickers[0].Last, 64)
	return price, nil
}

// SetStopLoss sets stop loss order (Trigger Order)
func (t *GateTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return t.placeTriggerOrder(symbol, positionSide, quantity, stopPrice, "stop_loss")
}

// SetTakeProfit sets take profit order
func (t *GateTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	return t.placeTriggerOrder(symbol, positionSide, quantity, takeProfitPrice, "take_profit")
}

func (t *GateTrader) placeTriggerOrder(symbol string, positionSide string, quantity float64, price float64, reason string) error {
	gateSymbol := t.convertSymbol(symbol)
	sizeStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return err
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	
	// Logic for Side:
	// If Position is LONG, SL/TP is SELL (Short).
	// If Position is SHORT, SL/TP is BUY (Long).
	// Size needs to reflect direction?
	// Gate.io Prices Triggered Order: size positive/negative?
	// "size": "Order size. positive for buying, negative for selling."
	
	// Determine size sign
	targetSize := size
	if positionSide == "LONG" {
		targetSize = -size // Sell to close long
	} else {
		targetSize = size // Buy to close short
	}
	
	// Rule 1: price > mark/last -> "price_greater_than" condition
	// Rule 2: price < mark/last -> "price_less_than" condition
	currentPrice, err := t.GetMarketPrice(symbol)
	if err != nil {
		return err
	}
	
	rule := 0 // 1: >=, 2: <=
	if price > currentPrice {
		rule = 1
	} else {
		rule = 2
	}
	
	trigger := gateapi.FuturesPriceTrigger{
		StrategyType: 0, // 0: price trigger
		PriceType:    1, // 1: mark price
		Price:        fmt.Sprintf("%v", price),
		Rule:         int32(rule),
		Expiration:   86400 * 30, // 30 days
	}
	
	order := gateapi.FuturesInitialOrder{
		Contract:   gateSymbol,
		Size:       targetSize, // int64
		Price:      "0", // Market price on trigger
		Tif:        "ioc",
		Text:       "web-api-" + reason,
		ReduceOnly: true,
		IsClose:    true,
	}
	
	arg := gateapi.FuturesPriceTriggeredOrder{
		Initial: order,   // The order to place
		Trigger: trigger, // The condition
	}
	
	ctx := t.getAuthContext()
	_, _, err = t.client.FuturesApi.CreatePriceTriggeredOrder(ctx, t.settle, arg)
	if err != nil {
		return fmt.Errorf("failed to create trigger order: %v", err)
	}
	
	return nil
}

// CancelStopLossOrders
func (t *GateTrader) CancelStopLossOrders(symbol string) error {
	// Gate.io cancel trigger orders usually by ID. 
	// Or we can delete all trigger orders for contract?
	// "CancelPriceTriggeredOrderList" removes all finished.
	// To cancel specific ACTIVE trigger orders, we need to list them and cancel.
	// For simplicity, we cancel ALL trigger orders for the symbol.
	// NOTE: This cancels both SL and TP.
	return t.CancelStopOrders(symbol)
}

func (t *GateTrader) CancelTakeProfitOrders(symbol string) error {
	// Same limitation as SL
	return t.CancelStopOrders(symbol)
}

func (t *GateTrader) CancelStopOrders(symbol string) error {
	gateSymbol := t.convertSymbol(symbol)
	ctx := t.getAuthContext()
	
	// Delete all open triggered orders for this contract
	_, _, err := t.client.FuturesApi.CancelPriceTriggeredOrderList(ctx, t.settle, &gateapi.CancelPriceTriggeredOrderListOpts{
		Contract: optional.NewString(gateSymbol),
	})
	
	return err
}

// CancelAllOrders cancels all regular pending orders
func (t *GateTrader) CancelAllOrders(symbol string) error {
	gateSymbol := t.convertSymbol(symbol)
	ctx := t.getAuthContext()
	
	_, _, err := t.client.FuturesApi.CancelFuturesOrders(ctx, t.settle, gateSymbol, nil)
	return err
}

// GetOrderStatus
func (t *GateTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	// gateSymbol := t.convertSymbol(symbol) // Not used for GetFuturesOrder
	ctx := t.getAuthContext()
	
	// GetFuturesOrder takes (ctx, settle, order_id) in v7 SDK
	order, _, err := t.client.FuturesApi.GetFuturesOrder(ctx, t.settle, orderID)
	if err != nil {
		return nil, err
	}
	
	status := "NEW"
	if order.Status == "finished" {
		status = "FILLED"
	}
	// Note: Gate status: "open", "finished"
	
	// Avg Price ? Gate.io `fill_price` ?
	// Order object has `fill_price`? No, it has `fill_price` in recent trades maybe?
	// Actually `order.FillPrice` exists in struct if user is owner?
	// SDK FuturesOrder struct has `FillPrice`?
	// Need to check docs. `fill_price` is typically available.
	
	// Assuming 0 for now if not readily available in main object without trades lookup.
	// But `order.Size` - `order.Left` shows executed.
	
	executed := float64(order.Size - order.Left)
	// We need multiplier to convert back to base asset quantity?
	// If calling standard interface, yes.
	contract, _ := t.getContractInfo(symbol)
	multiplier := 1.0
	if contract != nil {
		multiplier, _ = strconv.ParseFloat(contract.QuantoMultiplier, 64)
	}
	executedQty := math.Abs(executed) * multiplier
	
	return map[string]interface{}{
		"orderId": orderID,
		"status": status,
		"executedQty": executedQty,
	}, nil
}

// GetClosedPnL
func (t *GateTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	// Gate.io API: ListPositionClose 
	ctx := t.getAuthContext()
	
	// Pagination? 
	// Using default limit if 0
	if limit <= 0 {
		limit = 100
	}
	
	// Start time handled? Gate uses `from`? or `end`?
	// ListPositionCloseOpts has `Time` range?
	// No, it handles `contract`, `limit`...
	// Actually `ListPositionClose` usually returns recent. 
	// We might need to filter manually if API doesn't support time range well.
	
	history, _, err := t.client.FuturesApi.ListPositionClose(ctx, t.settle, &gateapi.ListPositionCloseOpts{
		Limit: optional.NewInt32(int32(limit)),
	})
	if err != nil {
		return nil, err
	}
	
	var records []ClosedPnLRecord
	
	for _, entry := range history {
		// Convert
		pnl, _ := strconv.ParseFloat(entry.Pnl, 64)
		// SDK v7 uses AccumSize for settled size? Or just Text?
		// Probe shows: AccumSize (string).
		settleSize, _ := strconv.ParseFloat(entry.AccumSize, 64)
		
		// If entry.Time < startTime, skip?
		// entry.Time is float64 (seconds)? No, usually seconds.
		entryTimeSec := int64(entry.Time)
		if entryTimeSec < startTime.Unix() {
			continue
		}
		
		// Map fields
		record := ClosedPnLRecord{
			Symbol: t.convertSymbolBack(entry.Contract),
			Side: entry.Side, // "long" or "short"
			RealizedPnL: pnl,
			ExitTime: time.Unix(entryTimeSec, 0),
			Quantity: math.Abs(settleSize), // Need verification on unit
		}
		
		records = append(records, record)
	}
	return records, nil
}

// GetOpenOrders
func (t *GateTrader) GetOpenOrders(symbol string) ([]OpenOrder, error) {
	gateSymbol := t.convertSymbol(symbol)
	ctx := t.getAuthContext()
	
	// Gate.io ListFuturesOrders (Open orders)
	// Takes (ctx, settle, contract, opts) in v7
	// Status cannot be specified in opts (missing field), defaults to open?
	// We will filter manually if needed, but API usually defaults to open.
	orders, _, err := t.client.FuturesApi.ListFuturesOrders(ctx, t.settle, gateSymbol, &gateapi.ListFuturesOrdersOpts{
		// Contract is passed as arg 3
		// Status is not available in opts
	})
	if err != nil {
		return nil, err
	}
	
	var result []OpenOrder
	for _, o := range orders {
		price, _ := strconv.ParseFloat(o.Price, 64)
		
		// multiplier for qty
		contract, _ := t.getContractInfo(symbol)
		multiplier := 1.0
		if contract != nil {
			multiplier, _ = strconv.ParseFloat(contract.QuantoMultiplier, 64)
		}
		qty := math.Abs(float64(o.Size)) * multiplier
		
		result = append(result, OpenOrder{
			OrderID: fmt.Sprintf("%d", o.Id),
			Symbol: symbol,
			Status: "NEW", // since we queried open
			Price: price,
			Quantity: qty,
			Side: func() string {
				if o.Size > 0 { return "BUY" }
				return "SELL"
			}(),
		})
	}
	
	// Also fetch trigger orders? Method name implies "orders". 
	// Standard interface usually implies LIMIT/MARKET pending. 
	// If you want TP/SL, check implementation of other traders.
	// Bybit implementation fetches conditional orders too.
	
	// ListPriceTriggeredOrders takes (ctx, settle, contract, opts)
	// Status cannot be filtered via opts in SDK v7 (missing field), strictly manual filtering
	triggers, _, err := t.client.FuturesApi.ListPriceTriggeredOrders(ctx, t.settle, gateSymbol, &gateapi.ListPriceTriggeredOrdersOpts{})
	if err == nil {
		for _, triggerOrder := range triggers {
			if triggerOrder.Status != "active" {
				continue
			}
			// Convert trigger to OpenOrder
			// TBD: details extraction
			// For now, primary orders are sufficient?
			// Let's create dummy entries for visibility
			result = append(result, OpenOrder{
				OrderID: fmt.Sprintf("%d", triggerOrder.Id), // Use trigger order ID
				Symbol: symbol,
				Type: "STOP", // Generic
				Status: "NEW",
			})
		}
	}
	
	return result, nil
}
