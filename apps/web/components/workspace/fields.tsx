"use client";

import type { ReactNode, RefObject } from "react";

import styles from "./workspace.module.css";

/**
 * The labelled form controls the workspace screens use.
 *
 * # Why these are not `components/auth/text-field.tsx`
 *
 * That component is deliberately built around `id={name}`, because the auth
 * error summary links to `#email` and a name is unique *within a form*. The
 * workspace screens break that assumption: `/app/projects/<id>` carries three
 * forms at once — rename, create a board, and confirm an archive — and two of
 * them have a field called `name`. Duplicate ids would point every `<label>` and
 * every summary link at whichever one the browser found first.
 *
 * So the id is a **prop**. Each form derives its ids from one `useId` (React's
 * documented pattern: one call, suffixed per field) and hands them both to the
 * field and to {@link FieldErrorList}, so the summary links where the label
 * points without either side guessing. That is the difference; everything else
 * about the accessibility contract is identical, and deliberately so:
 *
 * 1. a real `<label htmlFor>`, never a placeholder standing in for one;
 * 2. `aria-describedby` covering the hint *and* the error, so focusing a field
 *    announces both — which matters because focus is sent here after a failed
 *    submit;
 * 3. `aria-invalid` on the control, so the failure is in the accessibility tree
 *    and not only in a red border.
 */

type FieldShellProps = {
  id: string;
  label: string;
  hint?: ReactNode;
  error?: string;
  optional?: boolean;
  children: (wiring: {
    id: string;
    describedBy: string | undefined;
    invalid: boolean;
  }) => ReactNode;
};

/** The label, hint and error around a control, and the aria that ties them. */
function FieldShell({
  id,
  label,
  hint,
  error,
  optional = false,
  children,
}: FieldShellProps) {
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;

  const describedBy = [
    hint === undefined ? null : hintId,
    error === undefined ? null : errorId,
  ]
    .filter((value): value is string => value !== null)
    .join(" ");

  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={id}>
        {label}
        {optional && <span className={styles.optional}> (optional)</span>}
      </label>

      {hint !== undefined && (
        <p className={styles.hint} id={hintId}>
          {hint}
        </p>
      )}

      {children({
        id,
        describedBy: describedBy === "" ? undefined : describedBy,
        invalid: error !== undefined,
      })}

      {error !== undefined && (
        <p className={styles.fieldError} id={errorId}>
          {error}
        </p>
      )}
    </div>
  );
}

type CommonProps = {
  /** Unique on the page. Derive it from the form's `useId`. */
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  hint?: ReactNode;
  error?: string;
  optional?: boolean;
  disabled?: boolean;
};

export type TextFieldProps = CommonProps & {
  /**
   * `datetime-local` is here rather than in a field of its own because
   * everything around the control — the label, the hint, `aria-describedby`,
   * `aria-invalid` — is identical, and a second component would be that shell
   * copied so one attribute could change. Where the type is unsupported the
   * browser renders a text box, which is why `validateDueAt` in
   * `lib/workspace/rules.ts` still checks what comes out of it.
   */
  type?: "text" | "email" | "datetime-local";
  autoComplete?: string;
  inputRef?: RefObject<HTMLInputElement | null>;
  /**
   * A courtesy stop, not the rule. `lib/workspace/rules.ts` is the rule, and it
   * counts the way Go counts; `maxLength` counts UTF-16 units, so it is set
   * generously and never used as the check.
   */
  maxLength?: number;
};

export function TextField({
  id,
  label,
  value,
  onChange,
  type = "text",
  autoComplete,
  hint,
  error,
  optional,
  disabled = false,
  inputRef,
  maxLength,
}: TextFieldProps) {
  return (
    <FieldShell error={error} hint={hint} id={id} label={label} optional={optional}>
      {(wiring) => (
        <input
          aria-describedby={wiring.describedBy}
          aria-invalid={wiring.invalid}
          autoComplete={autoComplete}
          className={styles.input}
          disabled={disabled}
          id={wiring.id}
          maxLength={maxLength}
          onChange={(event) => onChange(event.target.value)}
          ref={inputRef}
          type={type}
          value={value}
        />
      )}
    </FieldShell>
  );
}

export type TextAreaFieldProps = CommonProps & { rows?: number; maxLength?: number };

export function TextAreaField({
  id,
  label,
  value,
  onChange,
  hint,
  error,
  optional,
  disabled = false,
  rows = 3,
  maxLength,
}: TextAreaFieldProps) {
  return (
    <FieldShell error={error} hint={hint} id={id} label={label} optional={optional}>
      {(wiring) => (
        <textarea
          aria-describedby={wiring.describedBy}
          aria-invalid={wiring.invalid}
          className={styles.textarea}
          disabled={disabled}
          id={wiring.id}
          maxLength={maxLength}
          onChange={(event) => onChange(event.target.value)}
          rows={rows}
          value={value}
        />
      )}
    </FieldShell>
  );
}

export type SelectFieldProps = CommonProps & {
  options: readonly { value: string; label: string }[];
};

export function SelectField({
  id,
  label,
  value,
  onChange,
  options,
  hint,
  error,
  disabled = false,
}: SelectFieldProps) {
  return (
    <FieldShell error={error} hint={hint} id={id} label={label}>
      {(wiring) => (
        <select
          aria-describedby={wiring.describedBy}
          className={styles.select}
          disabled={disabled}
          id={wiring.id}
          onChange={(event) => onChange(event.target.value)}
          value={value}
        >
          {options.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      )}
    </FieldShell>
  );
}

/**
 * The block above a form that says what went wrong, or what just happened.
 *
 * Same reasoning as `components/auth/form-alert.tsx`: `role="alert"` on an
 * element that is only *rendered* when there is something to say, because a
 * permanently mounted empty live region is announced inconsistently, and
 * `tabIndex={-1}` plus a ref so the form can move focus here after a failed
 * submit. The heading makes it something a screen reader user can navigate back
 * to.
 *
 * `tone="notice"` is for "this is not a failure, but read it" — a member who was
 * added, a project that was archived.
 */
export function FormMessage({
  title,
  children,
  tone = "error",
  messageRef,
}: {
  title: string;
  children: ReactNode;
  tone?: "error" | "notice";
  messageRef?: RefObject<HTMLDivElement | null>;
}) {
  return (
    <div
      className={`${styles.message}${tone === "notice" ? ` ${styles.messageNotice}` : ""}`}
      ref={messageRef}
      role="alert"
      tabIndex={-1}
    >
      <h3 className={styles.messageTitle}>{title}</h3>
      <div className={styles.messageBody}>{children}</div>
    </div>
  );
}

/**
 * Field problems inside a {@link FormMessage}, each linking to its control.
 *
 * An in-page `#id` link moves focus to the input in every current browser, so
 * "which box do I fix" is one key press away rather than a hunt down the form.
 */
export function FieldErrorList({
  errors,
}: {
  errors: readonly { id: string; message: string }[];
}) {
  return (
    <ul className={styles.messageList}>
      {errors.map((entry) => (
        <li key={entry.id}>
          <a href={`#${entry.id}`}>{entry.message}</a>
        </li>
      ))}
    </ul>
  );
}
