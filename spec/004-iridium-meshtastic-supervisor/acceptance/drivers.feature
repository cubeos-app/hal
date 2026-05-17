Feature: Iridium + Meshtastic driver supervisor (spec/004 — RETROSPECTIVE)

  # Covers: REQ-300, REQ-301, REQ-302, REQ-303, REQ-304, REQ-305, REQ-306, REQ-307, REQ-308, REQ-309, REQ-310, REQ-311, REQ-312, REQ-313, REQ-314, REQ-315, REQ-316, REQ-317, REQ-318, REQ-319, REQ-320

  Background:
    Given HAL is running with X-HAL-Key=core-key

  # Driver init
  Scenario: Driver initialised once per HAL process
    When HAL starts
    Then NewIridiumDriver and NewMeshtasticDriver each run exactly once
    And HALHandler holds them as h.iridium and h.meshtastic fields

  # Env disable
  Scenario: HAL_DISABLE_IRIDIUM=true skips driver init
    Given HAL_DISABLE_IRIDIUM=true
    When HAL starts
    Then h.iridium is nil
    And the log includes "Iridium driver disabled"

  Scenario: Disabled driver returns 501 on /iridium/*
    Given h.iridium is nil
    When GET /iridium/status is called
    Then HTTP 501 is returned with body "Iridium driver disabled (HAL_DISABLE_IRIDIUM=true)"

  # Transport multiplex
  Scenario: Meshtastic over BLE
    Given Meshtastic device address starts with "meshtastic://ble/"
    When POST /meshtastic/connect is called
    Then the BLE adapter (meshtastic_ble.go) handles the connection

  Scenario: Meshtastic over serial
    Given Meshtastic device address starts with "meshtastic://serial/"
    When POST /meshtastic/connect is called
    Then the serial adapter (meshtastic_serial.go) handles the connection

  # SSE
  Scenario: /meshtastic/events streams events as SSE
    Given a Meshtastic device is connected
    When GET /meshtastic/events is requested
    Then the response Content-Type is text/event-stream
    And events arrive as "data: <json>\n\n" lines

  Scenario: Client disconnect cleans up subscriber
    Given a client is connected to /iridium/events
    When the client closes the connection
    Then the subscriber goroutine exits within 5 seconds
    And no goroutine leak is detected

  # Admin commands
  Scenario: Meshtastic admin reboot is exposed
    When POST /meshtastic/admin/reboot is called
    Then the Meshtastic protocol-admin-reboot packet is sent

  # AT escape hatch
  Scenario: Iridium AT command escape hatch
    When POST /iridium/at with body "AT+CSQ" is called
    Then the AT command is sent to the modem
    And the response is returned in the body

  # Crash recovery
  Scenario: Driver panic is recovered by middleware
    Given the IridiumDriver handler triggers a panic
    When the panic occurs
    Then the Recovery middleware catches it
    And returns HTTP 500
    And HAL keeps running (process not killed)
