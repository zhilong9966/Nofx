package trader

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGateSymbolConversion(t *testing.T) {
	trader := &GateTrader{}

	tests := []struct {
		input  string
		expected string
	}{
		{"BTCUSDT", "BTC_USDT"},
		{"ETHUSDT", "ETH_USDT"},
		{"BTC_USDT", "BTC_USDT"}, // Already converted
		{"SOLUSD", "SOL_USD"},
		{"UNKNOWN", "UNKNOWN"},
	}

	for _, test := range tests {
		result := trader.convertSymbol(test.input)
		assert.Equal(t, test.expected, result, "Convert %s", test.input)
	}
}

func TestGateSymbolConversionBack(t *testing.T) {
	trader := &GateTrader{}

	tests := []struct {
		input  string
		expected string
	}{
		{"BTC_USDT", "BTCUSDT"},
		{"ETH_USDT", "ETHUSDT"},
		{"SOL_USD", "SOLUSD"},
		{"BTCUSDT", "BTCUSDT"}, // Already generic
	}

	for _, test := range tests {
		result := trader.convertSymbolBack(test.input)
		assert.Equal(t, test.expected, result, "Convert Back %s", test.input)
	}
}
