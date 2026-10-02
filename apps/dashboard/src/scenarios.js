// scenarios.js — the switchable stub behind the preview.
//
// One transport whose scenario can change at runtime, so the state gallery works without
// re-creating the dashboard (which would throw away the persistent coverage strip — the thing the
// gallery exists to show).
//
// In production this file is not used at all: `boot({api})` takes a real api over httpTransport.

import { createStubTransport, SCENARIOS, SCENARIO_NAMES } from './fixtures.js';

/**
 * A transport that answers from the named scenario, and can be pointed at another one.
 *
 * @param {string} [initial]
 */
export function scenarioTransport(initial = 'realistic') {
  let current = SCENARIO_NAMES.includes(initial) ? initial : 'realistic';
  // One stub per scenario, kept across requests: the stub tracks the cursors it has issued, and a
  // stub rebuilt per request would forget them and answer every page two with cursor_expired.
  let stub = createStubTransport(current);
  return Object.freeze({
    async send(body) {
      return stub.send(body);
    },
    /** Point the stub at another scenario. Returns the new one. */
    setScenario(name) {
      if (!SCENARIO_NAMES.includes(name) || name === current) return current;
      current = name;
      stub = createStubTransport(current);
      return current;
    },
    scenario() {
      return current;
    },
    label() {
      return SCENARIOS[current]?.label ?? current;
    },
  });
}

export { SCENARIOS, SCENARIO_NAMES };
