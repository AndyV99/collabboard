"use client";

import { type ReactNode, type RefObject, useId, useState } from "react";

import styles from "./auth.module.css";

/**
 * A labelled text input with its hint and its error wired to it.
 *
 * The accessibility here is the whole point of the component, and it is three
 * things a hand-rolled `<input>` reliably gets wrong:
 *
 * 1. **A real `<label htmlFor>`**, never a placeholder standing in for one. A
 *    placeholder disappears the moment there is a value, so it is not a label;
 *    it is a hint that vanishes when you most want to check it.
 * 2. **`aria-describedby` covering both the hint and the error**, so the field's
 *    requirements and what went wrong are announced when focus lands on it —
 *    which is exactly where focus is sent after a failed submit.
 * 3. **`aria-invalid` on the input itself**, so the failure is in the
 *    accessibility tree and not only in a red border.
 *
 * The ids are generated with `useId`, so two of these on one page cannot collide
 * and nothing has to invent a naming convention.
 */
export type TextFieldProps = {
  /** The `name` on the input, and the anchor the error summary links to. */
  name: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  type?: "text" | "email" | "password";
  /**
   * The autofill token. Always pass one: a sign-in form that browsers and
   * password managers cannot recognise is a form people retype by hand, which
   * pushes them towards passwords short enough to retype.
   */
  autoComplete?: string;
  hint?: ReactNode;
  error?: string;
  optional?: boolean;
  disabled?: boolean;
  inputRef?: RefObject<HTMLInputElement | null>;
  /** Adds a show/hide toggle. For the password fields. */
  revealable?: boolean;
};

export function TextField({
  name,
  label,
  value,
  onChange,
  type = "text",
  autoComplete,
  hint,
  error,
  optional = false,
  disabled = false,
  inputRef,
  revealable = false,
}: TextFieldProps) {
  const generatedId = useId();
  const [revealed, setRevealed] = useState(false);

  const hintId = `${generatedId}-hint`;
  const errorId = `${generatedId}-error`;

  const describedBy = [hint === undefined ? null : hintId, error === undefined ? null : errorId]
    .filter((id): id is string => id !== null)
    .join(" ");

  return (
    <div className={styles.field}>
      {/*
        The input's id is the field name rather than the generated one, because
        the error summary links to `#name` and a generated id is not something
        the summary can know. Names are unique within a form by definition.
      */}
      <label className={styles.label} htmlFor={name}>
        {label}
        {optional && <span className={styles.optional}> (optional)</span>}
      </label>

      {hint !== undefined && (
        <p className={styles.hint} id={hintId}>
          {hint}
        </p>
      )}

      <div className={styles.control}>
        <input
          aria-describedby={describedBy === "" ? undefined : describedBy}
          aria-invalid={error !== undefined}
          autoComplete={autoComplete}
          className={styles.input}
          disabled={disabled}
          id={name}
          name={name}
          onChange={(event) => onChange(event.target.value)}
          ref={inputRef}
          type={revealable && revealed ? "text" : type}
          value={value}
        />

        {revealable && (
          // The accessible name says what the button does *and* contains the
          // visible word, which is what WCAG 2.5.3 asks for — "Hide" is a
          // prefix of "Hide password", so speaking the visible label activates
          // the right control.
          <button
            aria-label={revealed ? "Hide password" : "Show password"}
            className={styles.reveal}
            disabled={disabled}
            onClick={() => setRevealed((shown) => !shown)}
            type="button"
          >
            {revealed ? "Hide" : "Show"}
          </button>
        )}
      </div>

      {error !== undefined && (
        <p className={styles.fieldError} id={errorId}>
          {error}
        </p>
      )}
    </div>
  );
}
