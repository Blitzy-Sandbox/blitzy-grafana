// NLQ feature: unit tests for NLQQueryPreview.
//
// Initial coverage focuses on the pure helper `parseCssLengthToPx` introduced
// to satisfy the MINOR review finding ("hardcoded Monaco values must resolve
// to GrafanaTheme2 tokens"). Broader component-level tests are deferred to
// the CP3 frontend implementation phase per AAP §0.6.1.4, which will exercise
// the full render path including theme integration with `useTheme2`.
//
// Why test the helper in isolation:
//   The Monaco fontSize/padding values are derived from GrafanaTheme2 strings
//   ("12px", "0.75rem") and Monaco requires numeric pixels. The helper bridges
//   the two representations. A regression in the parser would silently fall
//   back to the default pixel constants, defeating the design-system fix —
//   so the parser invariants are pinned with direct unit tests.

import { parseCssLengthToPx } from './NLQQueryPreview';

describe('parseCssLengthToPx', () => {
  it('parses a pixel string like "12px"', () => {
    expect(parseCssLengthToPx('12px', 99)).toBe(12);
  });

  it('parses a fractional pixel string like "13.5px"', () => {
    expect(parseCssLengthToPx('13.5px', 99)).toBe(13.5);
  });

  it('parses a rem string using the standard 16px-per-rem ratio', () => {
    // 0.75rem * 16 == 12px
    expect(parseCssLengthToPx('0.75rem', 99)).toBe(12);
  });

  it('parses a whitespace-padded value', () => {
    expect(parseCssLengthToPx('  14px  ', 99)).toBe(14);
  });

  it('parses a bare numeric string as pixels', () => {
    expect(parseCssLengthToPx('16', 99)).toBe(16);
  });

  it('passes through a numeric input unchanged', () => {
    expect(parseCssLengthToPx(20, 99)).toBe(20);
  });

  it('falls back to the default when value is undefined', () => {
    expect(parseCssLengthToPx(undefined, 99)).toBe(99);
  });

  it('falls back to the default for an unrecognised unit', () => {
    // "12em" is intentionally NOT supported — em depends on the parent
    // font size which Monaco cannot compute. The fallback prevents the
    // helper from silently producing a nonsense pixel count.
    expect(parseCssLengthToPx('12em', 99)).toBe(99);
  });

  it('falls back to the default for a non-numeric string', () => {
    expect(parseCssLengthToPx('not-a-length', 99)).toBe(99);
  });

  it('falls back to the default for NaN inputs', () => {
    expect(parseCssLengthToPx(NaN, 99)).toBe(99);
  });
});
