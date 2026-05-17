Feature: UPS + PowerMonitor (spec/005 — RETROSPECTIVE)

  # Covers: REQ-400, REQ-401, REQ-402, REQ-403, REQ-404, REQ-405, REQ-406, REQ-407, REQ-408, REQ-409, REQ-410, REQ-411, REQ-412, REQ-413, REQ-414, REQ-415, REQ-416, REQ-417, REQ-418

  Background:
    Given HAL is running with X-HAL-Key=core-key

  # Detection
  Scenario: Detect PiSugar3
    Given a PiSugar3 hat is mounted on I2C 0x57
    When GET /power/ups/detect is called
    Then the response is {model: "PiSugar3", i2c_addr: 87, driver: "pisugar3"}

  Scenario: Detect X1202
    Given a Geekworm X1202 hat is mounted on I2C 0x36
    When GET /power/ups/detect is called
    Then the response is {model: "X1202", i2c_addr: 54, driver: "x1202"}

  Scenario: No UPS detected
    Given no UPS hat is present
    When GET /power/ups/detect is called
    Then the response is {detected: false}

  # Multi-driver pick
  Scenario: PiSugar3 wins when multiple hats coexist
    Given PiSugar3 AND X728 are both responding
    When detection runs
    Then driver="pisugar3" is selected

  # Configure
  Scenario: Configure persists to /cubeos/config/ups.json
    When POST /power/ups/configure {driver: "x1202", i2c_addr: 54} is called
    Then /cubeos/config/ups.json contains active_driver="x1202", i2c_addr=54

  # Battery readout
  Scenario: ReadBattery returns SOC + voltage + state
    Given PiSugar3 is the active driver
    When GET /power/battery is called
    Then the response includes percent_soc, voltage_v, charging, on_battery, time_to_empty_min or time_to_full_min

  # PowerMonitor
  Scenario: Monitor start begins polling at 5s interval
    When POST /power/monitor/start is called
    Then PowerMonitor starts
    And GET /power/monitor/status shows running=true + last_read_at advances every 5 seconds

  Scenario: Monitor stop terminates the goroutine
    Given monitor is running
    When POST /power/monitor/stop is called
    Then PowerMonitor stops within 1 second
    And GET /power/monitor/status shows running=false

  Scenario: Critical battery logs CRITICAL event
    Given the active UPS reports 4% SOC and threshold is 5%
    When PowerMonitor polls
    Then a CRITICAL event is logged

  # Charging control
  Scenario: SetCharging works for supporting drivers
    Given the active driver is X1202 (supports charging control)
    When POST /power/charging {enabled: false} is called
    Then the X1202 charging state changes to disabled

  Scenario: SetCharging returns 501 for unsupporting drivers
    Given the active driver does NOT support charging control
    When POST /power/charging is called
    Then HTTP 501 is returned with body "driver does not support charging control"
