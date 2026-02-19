#!/bin/bash
# CubeOS HAL Power Management Integration Tests
# Run this on a Pi with Geekworm X1202 UPS HAT connected
#
# Usage: ./test-power.sh [HAL_URL]

HAL_URL="${1:-http://127.0.0.1:6005}"
PASS=0
FAIL=0

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=============================================="
echo "  CubeOS HAL Power Management Tests"
echo "  HAL URL: $HAL_URL"
echo "  Date: $(date)"
echo "=============================================="
echo ""

# Test helper function
test_endpoint() {
    local name="$1"
    local method="$2"
    local endpoint="$3"
    local expected_field="$4"
    local data="$5"
    
    echo -n "Testing: $name... "
    
    if [ "$method" == "GET" ]; then
        response=$(curl -sf "$HAL_URL$endpoint" 2>/dev/null) || response=""
    else
        response=$(curl -sf -X "$method" -H "Content-Type: application/json" -d "$data" "$HAL_URL$endpoint" 2>/dev/null) || response=""
    fi
    
    if [ -z "$response" ]; then
        echo -e "${RED}FAIL${NC} (no response)"
        FAIL=$((FAIL+1))
        return 1
    fi
    
    if [ -n "$expected_field" ]; then
        if echo "$response" | jq -e ".$expected_field" > /dev/null 2>&1; then
            echo -e "${GREEN}PASS${NC}"
            PASS=$((PASS+1))
            return 0
        else
            echo -e "${RED}FAIL${NC} (missing field: $expected_field)"
            echo "  Response: $response"
            FAIL=$((FAIL+1))
            return 1
        fi
    else
        echo -e "${GREEN}PASS${NC}"
        PASS=$((PASS+1))
        return 0
    fi
}

# ============================================
# Health Check
# ============================================
echo "--- Health Check ---"
test_endpoint "HAL health" "GET" "/health" "status"

# ============================================
# Power Status (comprehensive)
# ============================================
echo ""
echo "--- Power Status ---"
test_endpoint "Full power status" "GET" "/hal/power/status" "ups"
test_endpoint "Battery status" "GET" "/hal/power/battery" "available"
test_endpoint "UPS info" "GET" "/hal/power/ups" "model"

# ============================================
# System Info
# ============================================
echo ""
echo "--- System Info ---"
test_endpoint "System uptime" "GET" "/hal/system/uptime" "seconds"
test_endpoint "CPU throttle" "GET" "/hal/system/throttle" "raw"
test_endpoint "CPU temperature" "GET" "/hal/system/temperature" "temperature"

# ============================================
# RTC
# ============================================
echo ""
echo "--- RTC (Real-Time Clock) ---"
test_endpoint "RTC status" "GET" "/hal/rtc/status" "available"

# ============================================
# Watchdog
# ============================================
echo ""
echo "--- Watchdog ---"
test_endpoint "Watchdog status" "GET" "/hal/watchdog/status" "available"

# ============================================
# I2C
# ============================================
echo ""
echo "--- I2C Bus ---"
test_endpoint "List I2C buses" "GET" "/hal/i2c/buses" "buses"
test_endpoint "Scan I2C bus 1" "GET" "/hal/i2c/scan?bus=1" "devices"

# ============================================
# Network
# ============================================
echo ""
echo "--- Network ---"
test_endpoint "Network status" "GET" "/hal/network/status" "internet"
test_endpoint "Network interfaces" "GET" "/hal/network/interfaces" "interfaces"
test_endpoint "AP clients" "GET" "/hal/network/ap/clients" "clients"
test_endpoint "Interface traffic" "GET" "/hal/network/interface/wlan0/traffic" "stats"

# ============================================
# Detailed Output
# ============================================
echo ""
echo "=============================================="
echo "  Detailed Status Output"
echo "=============================================="

echo ""
echo "--- Battery Status ---"
curl -s "$HAL_URL/hal/power/battery" | jq '.' 2>/dev/null || echo "Failed to get battery status"

echo ""
echo "--- UPS Info ---"
curl -s "$HAL_URL/hal/power/ups" | jq '.' 2>/dev/null || echo "Failed to get UPS info"

echo ""
echo "--- CPU Throttle ---"
curl -s "$HAL_URL/hal/system/throttle" | jq '.' 2>/dev/null || echo "Failed to get throttle status"

echo ""
echo "--- CPU Temperature ---"
curl -s "$HAL_URL/hal/system/temperature" | jq '.' 2>/dev/null || echo "Failed to get temperature"

echo ""
echo "--- Uptime ---"
curl -s "$HAL_URL/hal/system/uptime" | jq '.' 2>/dev/null || echo "Failed to get uptime"

echo ""
echo "--- RTC ---"
curl -s "$HAL_URL/hal/rtc/status" | jq '.' 2>/dev/null || echo "Failed to get RTC status"

echo ""
echo "--- Watchdog ---"
curl -s "$HAL_URL/hal/watchdog/status" | jq '.' 2>/dev/null || echo "Failed to get watchdog status"

echo ""
echo "--- I2C Devices on Bus 1 ---"
curl -s "$HAL_URL/hal/i2c/scan?bus=1" | jq '.' 2>/dev/null || echo "Failed to scan I2C"

echo ""
echo "--- Network Status ---"
curl -s "$HAL_URL/hal/network/status" | jq '{internet, internet_latency, ap, nat, firewall}' 2>/dev/null || echo "Failed to get network status"

# ============================================
# Summary
# ============================================
echo ""
echo "=============================================="
echo "  Test Summary"
echo "=============================================="
echo -e "  ${GREEN}Passed:${NC} $PASS"
echo -e "  ${RED}Failed:${NC} $FAIL"
echo "=============================================="

if [ $FAIL -gt 0 ]; then
    echo -e "${YELLOW}Some tests failed. Check if:${NC}"
    echo "  1. HAL service is running: docker ps | grep hal"
    echo "  2. UPS HAT is connected and I2C enabled: i2cdetect -y 1"
    echo "  3. Correct permissions for /dev/i2c-1, /dev/gpiochip4"
    exit 1
fi

echo -e "${GREEN}All tests passed!${NC}"
exit 0
