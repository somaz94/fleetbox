import { absolute, ago, shortDigest, shortID } from "@/lib/format";

test("shortID keeps 12 characters", () => {
  expect(shortID("0123456789abcdef")).toBe("0123456789ab");
  expect(shortID("abc")).toBe("abc");
});

test("shortDigest keeps the algorithm and 12 hex characters", () => {
  expect(shortDigest("sha256:0123456789abcdef0123")).toBe("sha256:0123456789ab");
  expect(shortDigest("0123456789abcdef")).toBe("0123456789ab");
  expect(shortDigest(undefined)).toBe("");
});

test("ago is relative to now", () => {
  const now = new Date("2026-01-01T12:00:00Z");
  expect(ago("2026-01-01T11:58:00Z", now)).toBe("2 minutes ago");
  expect(ago("2025-12-30T12:00:00Z", now)).toBe("2 days ago");
  expect(ago("2026-01-01T12:00:00Z", now)).toBe("now");
  expect(ago(undefined, now)).toBe("never");
  expect(absolute(undefined)).toBe("");
  expect(absolute("2026-01-01T12:00:00Z")).not.toBe("");
});
