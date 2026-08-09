/**
 * The client-side validation, checked against the numbers `apps/api` enforces.
 *
 * The interesting assertions here are the ones about *counting*: a name that is
 * exactly 200 emoji is 200 runes to Go and 400 UTF-16 units to a naive
 * `.length`, so a rule written with `.length` would reject a name the API is
 * happy with — which is the failure mode `lib/auth/rules.ts` exists to warn
 * about, restated for a different pair of fields.
 */

import { describe, expect, it } from "vitest";

import {
  MAX_DESCRIPTION_CODE_POINTS,
  MAX_EMAIL_BYTES,
  MAX_NAME_CODE_POINTS,
  projectChanged,
  validateDescription,
  validateMemberEmail,
  validateName,
} from "@/lib/workspace/rules";

describe("validateName", () => {
  it("accepts an ordinary name", () => {
    expect(validateName("Launch", "Project")).toBeUndefined();
  });

  it("rejects a blank name, and treats whitespace as blank", () => {
    // `requiredText` trims before it checks, so "   " is a blank name on the
    // API too — and would otherwise be stored as a name that renders as nothing.
    expect(validateName("", "Project")).toBeDefined();
    expect(validateName("   ", "Project")).toBeDefined();
    expect(validateName("\n\t ", "Board")).toBeDefined();
  });

  it("names the subject in the message", () => {
    expect(validateName("", "Board")).toContain("board");
    expect(validateName("", "Project")).toContain("project");
  });

  it("accepts a name of exactly the maximum length", () => {
    expect(validateName("a".repeat(MAX_NAME_CODE_POINTS), "Project")).toBeUndefined();
  });

  it("rejects one code point past the maximum", () => {
    expect(validateName("a".repeat(MAX_NAME_CODE_POINTS + 1), "Project")).toBeDefined();
  });

  it("counts code points, not UTF-16 units", () => {
    // 200 astral-plane characters: 200 runes to Go, 400 to String.length. A
    // rule written with .length would reject a name the API accepts.
    const emoji = "🔐".repeat(MAX_NAME_CODE_POINTS);

    expect(emoji.length).toBe(MAX_NAME_CODE_POINTS * 2);
    expect(validateName(emoji, "Project")).toBeUndefined();
  });

  it("counts the trimmed value, as the API does", () => {
    const padded = `  ${"a".repeat(MAX_NAME_CODE_POINTS)}  `;

    expect(validateName(padded, "Project")).toBeUndefined();
  });
});

describe("validateDescription", () => {
  it("accepts an empty description, because the API does", () => {
    // `boundedText` allows empty and `optionalText(..., allowEmpty: true)` lets
    // a PATCH clear one. A required description here would be a rule with no
    // authority behind it.
    expect(validateDescription("")).toBeUndefined();
    expect(validateDescription("   ")).toBeUndefined();
  });

  it("rejects one past the maximum", () => {
    expect(validateDescription("a".repeat(MAX_DESCRIPTION_CODE_POINTS))).toBeUndefined();
    expect(validateDescription("a".repeat(MAX_DESCRIPTION_CODE_POINTS + 1))).toBeDefined();
  });
});

describe("validateMemberEmail", () => {
  it("accepts an ordinary address", () => {
    expect(validateMemberEmail("colleague@example.com")).toBeUndefined();
  });

  it("requires something before the @", () => {
    // Not a stricter rule than the API's: `users.email` carries
    // CHECK (position('@' IN email) > 1), so no account can have such an
    // address and submitting one could only ever produce the 404 that says
    // "no account with that email address".
    expect(validateMemberEmail("@example.com")).toBeDefined();
  });

  it("requires an @ at all", () => {
    expect(validateMemberEmail("colleague")).toBeDefined();
  });

  it("rejects a blank address", () => {
    expect(validateMemberEmail("")).toBeDefined();
    expect(validateMemberEmail("  ")).toBeDefined();
  });

  it("counts bytes, as Go's len() does", () => {
    const local = "a".repeat(MAX_EMAIL_BYTES - "@example.com".length);

    expect(validateMemberEmail(`${local}@example.com`)).toBeUndefined();
    expect(validateMemberEmail(`${local}a@example.com`)).toBeDefined();
  });
});

describe("projectChanged", () => {
  const project = { name: "Launch", description: "Q3" };

  it("is false when nothing was edited", () => {
    // A PATCH mentioning neither field is a 400 on the API. Pressing Save on an
    // untouched form should not collect one.
    expect(projectChanged(project, { name: "Launch", description: "Q3" })).toBe(false);
  });

  it("is false when only whitespace was added, because the API trims", () => {
    expect(projectChanged(project, { name: " Launch ", description: " Q3 " })).toBe(false);
  });

  it("is true for a rename", () => {
    expect(projectChanged(project, { name: "Relaunch", description: "Q3" })).toBe(true);
  });

  it("is true when the description is being cleared", () => {
    expect(projectChanged(project, { name: "Launch", description: "" })).toBe(true);
  });
});
