Feature: Watchdog + RTC subsystem (spec/002 — RETROSPECTIVE)

  # Covers: REQ-100, REQ-101, REQ-102, REQ-103, REQ-104, REQ-105, REQ-106, REQ-107, REQ-108, REQ-109, REQ-110, REQ-111, REQ-112, REQ-113, REQ-114, REQ-115

  Background:
    Given HAL is running with X-HAL-Key=core-key

  # Watchdog
  Scenario: Enable watchdog opens /dev/watchdog and starts pet loop
    When POST /watchdog/enable is called
    Then HTTP 200 is returned
    And the pet loop pets /dev/watchdog every 5 seconds

  Scenario: /dev/watchdog missing returns 503
    Given /dev/watchdog is not present (kernel module not loaded)
    When POST /watchdog/enable is called
    Then HTTP 503 is returned with body explaining the missing device

  Scenario: One-shot pet works
    Given watchdog is enabled
    When POST /watchdog/pet is called
    Then HTTP 200 is returned
    And the lastPetAt timestamp is updated

  Scenario: SIGTERM releases /dev/watchdog
    Given watchdog is enabled
    When HAL receives SIGTERM
    Then /dev/watchdog fd is closed before process exit
    And the kernel does NOT reset the board

  # RTC
  Scenario: /rtc/status reports RTC time vs system time drift
    When GET /rtc/status is called
    Then the response includes rtc_time, system_time, drift_seconds, and rtc_present=true

  Scenario: sync-to-rtc writes system time to RTC
    When POST /rtc/sync-to-rtc is called
    Then `hwclock --set` is invoked with the current system time
    And HTTP 200 is returned

  Scenario: wakealarm POST sets the alarm
    When POST /rtc/wakealarm with body {"epoch": 1747396800} is called
    Then /sys/class/rtc/rtc0/wakealarm receives "0" then "1747396800"

  Scenario: wakealarm DELETE clears
    Given a wakealarm is set
    When DELETE /rtc/wakealarm is called
    Then /sys/class/rtc/rtc0/wakealarm receives "0"

  Scenario: No RTC chip returns 503
    Given /sys/class/rtc/rtc0/ does not exist
    When GET /rtc/status is called
    Then HTTP 503 is returned with body explaining no RTC hardware
