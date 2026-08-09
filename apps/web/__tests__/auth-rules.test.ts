/**
 * The client-side rules, checked against the numbers `apps/api` enforces.
 *
 * The point of these tests is not that the validation works — it is that it is
 * not *stricter* than the server. Every "accepts" case below is a value the Go
 * code accepts, so a tightened rule here fails a test rather than quietly
 * rejecting a password the API would have taken.
 */

import { describe, expect, it } from "vitest";

import {
  MAX_DISPLAY_NAME_CODE_POINTS,
  MAX_EMAIL_BYTES,
  MAX_PASSWORD_BYTES,
  MIN_PASSWORD_BYTES,
  byteLength,
  codePointLength,
  defaultOrganizationName,
  firstInvalidField,
  hasErrors,
  LOGIN_FIELD_ORDER,
  normalizeEmail,
  REGISTRATION_FIELD_ORDER,
  validateLogin,
  validateRegistration,
} from "@/lib/auth/rules";

const valid = {
  email: "andy@example.com",
  password: "correct horse battery",
  displayName: "Andy",
  organizationName: "",
};

describe("the constants mirror apps/api", () => {
  it("uses the API's numbers", () => {
    // `MinPasswordLength`, `MaxPasswordLength`, `maxEmailLength` and
    // `maxDisplayNameLength` in apps/api/internal/auth/service.go.
    expect(MIN_PASSWORD_BYTES).toBe(12);
    expect(MAX_PASSWORD_BYTES).toBe(128);
    expect(MAX_EMAIL_BYTES).toBe(254);
    expect(MAX_DISPLAY_NAME_CODE_POINTS).toBe(128);
  });
});

describe("lengths are counted the way Go counts them", () => {
  it("counts passwords in bytes, not UTF-16 units", () => {
    // Six astral-plane characters: `String.length` says 12, Go's `len` says 24.
    // Counting code units would let a 24-byte password through a >= 12 check by
    // accident, and would reject a shorter one for the same reason.
    const emoji = "🔐🔐🔐🔐🔐🔐";

    expect(emoji.length).toBe(12);
    expect(byteLength(emoji)).toBe(24);
    expect(validateRegistration({ ...valid, password: emoji }).password).toBeUndefined();
  });

  it("rejects a password whose byte length is short even when it looks long", () => {
    // Eleven ASCII characters is eleven bytes, whatever it looks like.
    expect(validateRegistration({ ...valid, password: "elevenchars" }).password).toContain(
      "at least 12",
    );
  });

  it("counts display names in code points, matching utf8.RuneCountInString", () => {
    const name = "é".repeat(MAX_DISPLAY_NAME_CODE_POINTS);

    expect(codePointLength(name)).toBe(MAX_DISPLAY_NAME_CODE_POINTS);
    expect(byteLength(name)).toBe(MAX_DISPLAY_NAME_CODE_POINTS * 2);
    expect(validateRegistration({ ...valid, displayName: name }).displayName).toBeUndefined();
    expect(
      validateRegistration({ ...valid, displayName: `${name}x` }).displayName,
    ).toBeDefined();
  });

  it("accepts a password exactly at each bound", () => {
    expect(
      validateRegistration({ ...valid, password: "a".repeat(MIN_PASSWORD_BYTES) }).password,
    ).toBeUndefined();
    expect(
      validateRegistration({ ...valid, password: "a".repeat(MAX_PASSWORD_BYTES) }).password,
    ).toBeUndefined();
    expect(
      validateRegistration({ ...valid, password: "a".repeat(MAX_PASSWORD_BYTES + 1) }).password,
    ).toBeDefined();
  });
});

describe("no rule the API does not have", () => {
  it("asks for no symbols, digits or capitals", () => {
    // The API states this as a design decision: length over composition, because
    // composition rules produce "Password1!" and reuse.
    expect(hasErrors(validateRegistration({ ...valid, password: "aaaaaaaaaaaa" }))).toBe(false);
  });

  it("accepts an address the browser's type=email would refuse", () => {
    // `a@b` has no dot. The API asks only for an `@` with something before it,
    // and this is why the forms carry `noValidate`.
    expect(validateRegistration({ ...valid, email: "a@b" }).email).toBeUndefined();
  });

  it("accepts a workspace name left blank", () => {
    expect(validateRegistration({ ...valid, organizationName: "" }).organizationName)
      .toBeUndefined();
  });
});

describe("the two constraints the Go service does not check", () => {
  it("refuses an address with nothing before the @", () => {
    // `users.email` carries CHECK (position('@' IN email) > 1), so the service
    // accepts `@example.com` and the insert then fails with a 500. Refusing it
    // here is the database's rule, not a stricter one.
    expect(validateRegistration({ ...valid, email: "@example.com" }).email).toBeDefined();
  });

  it("refuses an address past the length the column will take", () => {
    const long = `${"a".repeat(MAX_EMAIL_BYTES)}@example.com`;

    expect(validateRegistration({ ...valid, email: long }).email).toBeDefined();
  });
});

describe("normalisation matches auth.NormalizeEmail", () => {
  it("trims and lower-cases, and nothing else", () => {
    expect(normalizeEmail("  Andy@Example.COM ")).toBe("andy@example.com");
    expect(normalizeEmail("a+tag@example.com")).toBe("a+tag@example.com");
  });

  it("validates the normalised form", () => {
    expect(validateRegistration({ ...valid, email: "  ANDY@EXAMPLE.COM  " }).email)
      .toBeUndefined();
  });
});

describe("login validates presence and nothing else", () => {
  it("accepts a password far too short to have been registered", () => {
    // An account made under an older rule still has to be able to sign in, and
    // "too short to be one of ours" is a statement about a stored value.
    expect(validateLogin({ email: "andy@example.com", password: "x" })).toEqual({});
  });

  it("accepts an address shape it would refuse on sign-up", () => {
    expect(validateLogin({ email: "@example.com", password: "x" })).toEqual({});
  });

  it("catches an empty form before it costs a rate-limit slot", () => {
    const errors = validateLogin({ email: "   ", password: "" });

    expect(errors.email).toBeDefined();
    expect(errors.password).toBeDefined();
  });
});

describe("error ordering", () => {
  it("reports the first problem in screen order", () => {
    const errors = validateRegistration({
      email: "",
      password: "short",
      displayName: "",
      organizationName: "",
    });

    expect(firstInvalidField(errors, REGISTRATION_FIELD_ORDER)).toBe("email");
    expect(firstInvalidField({ password: "x" }, REGISTRATION_FIELD_ORDER)).toBe("password");
    expect(firstInvalidField({}, LOGIN_FIELD_ORDER)).toBeNull();
  });
});

describe("the workspace default", () => {
  it("matches what Register invents when the name is blank", () => {
    expect(defaultOrganizationName("Andy")).toBe("Andy's workspace");
    expect(defaultOrganizationName("  Andy  ")).toBe("Andy's workspace");
    expect(defaultOrganizationName("   ")).toBe("");
  });
});
