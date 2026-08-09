"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useRef, useState } from "react";

import type { AuthFailure } from "@/lib/auth/outcomes";
import { describeLoginFailure, describeRegistrationFailure } from "@/lib/auth/outcomes";
import {
  type FieldErrors,
  type RegistrationField,
  MIN_PASSWORD_BYTES,
  REGISTRATION_FIELD_ORDER,
  defaultOrganizationName,
  hasErrors,
  normalizeEmail,
  validateRegistration,
} from "@/lib/auth/rules";
import { signInHref } from "@/lib/auth/routes";
import { LOGIN_PATH, REGISTER_PATH, submitAuth } from "@/lib/auth/submit";
import { FieldErrorList, FormAlert } from "./form-alert";
import { TextField } from "./text-field";
import styles from "./auth.module.css";

const FIELD_LABELS: Record<RegistrationField, string> = {
  email: "Email address",
  password: "Password",
  displayName: "Your name",
  organizationName: "Workspace name",
};

/**
 * The sign-up form. One submit creates an account, a workspace, and a session.
 *
 * # Three requests, one button
 *
 * `POST /auth/register` does not return tokens — registering is not signing in,
 * and `app/api/auth/register` relays that faithfully rather than minting a
 * session behind the user's back. So this form posts to `/api/auth/register`
 * and then, on 201, to `/api/auth/login` with the same credentials. The user
 * sees one action, and "a session exists" stays the result of presenting a
 * password.
 *
 * The interesting case is when the second request fails — a 429 is the likely
 * one, because both calls count against the same per-address budget. The
 * account exists at that point, so the form says so and offers the sign-in
 * screen. What it must not do is look like the whole thing failed, because the
 * user's next move would be to sign up again and collect a 409.
 *
 * # Registration *does* disclose that an address is taken
 *
 * 409, deliberately, and `registerHandler` in `apps/api` explains why: the
 * alternative is a confirmation email this service cannot send, and a silent
 * 201 would leave a user unable to tell a typo from a taken address. So this
 * form relays it plainly. Login, which is the endpoint an attacker enumerates
 * against at scale, still gives nothing away — see `login-form.tsx`.
 *
 * # The rules under the fields are the API's rules
 *
 * Twelve characters and no composition requirements, because that is what
 * `apps/api/internal/auth` enforces and it enforces it for a stated reason.
 * Inventing a "must contain a symbol" rule here would reject passwords the
 * server would have taken, and would push people towards `Password1!`.
 */
export function RegisterForm({ returnTo }: { returnTo: string }) {
  const router = useRouter();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [organizationName, setOrganizationName] = useState("");

  const [fieldErrors, setFieldErrors] = useState<FieldErrors<RegistrationField>>({});
  const [failure, setFailure] = useState<AuthFailure | null>(null);
  const [createdNeedsSignIn, setCreatedNeedsSignIn] = useState(false);
  const [pending, setPending] = useState(false);
  const [blockedUntil, setBlockedUntil] = useState<number | null>(null);

  const [attempt, setAttempt] = useState(0);
  const alertRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      alertRef.current?.focus();
    }
  }, [attempt]);

  useEffect(() => {
    if (blockedUntil === null) {
      return;
    }

    const timer = setTimeout(
      () => setBlockedUntil(null),
      Math.max(0, blockedUntil - Date.now()),
    );

    return () => clearTimeout(timer);
  }, [blockedUntil]);

  const rateLimited = blockedUntil !== null;

  function announce(
    errors: FieldErrors<RegistrationField>,
    nextFailure: AuthFailure | null,
    created = false,
  ) {
    setFieldErrors(errors);
    setFailure(nextFailure);
    setCreatedNeedsSignIn(created);
    setAttempt((count) => count + 1);
  }

  function blockFor(nextFailure: AuthFailure) {
    if (nextFailure.retryAfterSeconds !== undefined) {
      setBlockedUntil(Date.now() + nextFailure.retryAfterSeconds * 1000);
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending || rateLimited) {
      return;
    }

    const fields = { email, password, displayName, organizationName };
    const errors = validateRegistration(fields);

    if (hasErrors(errors)) {
      announce(errors, null);

      return;
    }

    setPending(true);
    setFieldErrors({});
    setFailure(null);
    setCreatedNeedsSignIn(false);

    const normalizedEmail = normalizeEmail(email);
    const trimmedOrganization = organizationName.trim();

    const registered = await submitAuth(
      REGISTER_PATH,
      {
        email: normalizedEmail,
        password,
        display_name: displayName.trim(),
        // Omitted rather than sent empty, so the API applies its own default
        // ("<name>'s workspace") instead of this app duplicating it.
        ...(trimmedOrganization === ""
          ? {}
          : { organization_name: trimmedOrganization }),
      },
      describeRegistrationFailure,
    );

    if (!registered.ok) {
      setPending(false);
      announce({}, registered.failure);
      blockFor(registered.failure);

      return;
    }

    const signedIn = await submitAuth(
      LOGIN_PATH,
      { email: normalizedEmail, password },
      describeLoginFailure,
    );

    if (!signedIn.ok) {
      setPending(false);
      // The account exists. Say so, and send them to sign in rather than
      // letting them try to create it again.
      announce({}, signedIn.failure, true);
      blockFor(signedIn.failure);

      return;
    }

    router.replace(returnTo);
    router.refresh();
  }

  const listedErrors = REGISTRATION_FIELD_ORDER.filter(
    (field) => fieldErrors[field] !== undefined,
  ).map((field) => ({
    field,
    message: `${FIELD_LABELS[field]}: ${fieldErrors[field]}`,
  }));

  const workspaceDefault = defaultOrganizationName(displayName);

  return (
    <>
      {listedErrors.length > 0 && (
        <FormAlert alertRef={alertRef} title="Check the form">
          <FieldErrorList errors={listedErrors} />
        </FormAlert>
      )}

      {listedErrors.length === 0 && createdNeedsSignIn && failure !== null && (
        <FormAlert alertRef={alertRef} title="Your account was created" tone="notice">
          <p>
            {failure.message} Your account already exists, so{" "}
            <Link href={signInHref(returnTo)}>sign in</Link> rather than signing
            up again.
          </p>
        </FormAlert>
      )}

      {listedErrors.length === 0 && !createdNeedsSignIn && failure !== null && (
        <FormAlert alertRef={alertRef} title="Could not create your account">
          <p>
            {failure.message}
            {failure.kind === "email_taken" && (
              <>
                {" "}
                <Link href={signInHref(returnTo)}>Sign in instead</Link>.
              </>
            )}
            {failure.kind === "unconfirmed" && (
              <>
                {" "}
                <Link href={signInHref(returnTo)}>Try signing in</Link>.
              </>
            )}
          </p>
        </FormAlert>
      )}

      <form className={styles.form} noValidate onSubmit={handleSubmit}>
        <TextField
          autoComplete="email"
          disabled={pending}
          error={fieldErrors.email}
          label={FIELD_LABELS.email}
          name="email"
          onChange={setEmail}
          type="email"
          value={email}
        />

        <TextField
          autoComplete="new-password"
          disabled={pending}
          error={fieldErrors.password}
          hint={`At least ${MIN_PASSWORD_BYTES} characters. Length is all that is required — no symbols, no digits, no capitals.`}
          label={FIELD_LABELS.password}
          name="password"
          onChange={setPassword}
          revealable
          type="password"
          value={password}
        />

        <TextField
          autoComplete="name"
          disabled={pending}
          error={fieldErrors.displayName}
          hint="Shown to everyone in your workspace."
          label={FIELD_LABELS.displayName}
          name="displayName"
          onChange={setDisplayName}
          value={displayName}
        />

        <TextField
          autoComplete="organization"
          disabled={pending}
          error={fieldErrors.organizationName}
          hint={
            workspaceDefault === ""
              ? "Your projects and boards live in a workspace. Leave this blank and one is named after you."
              : `Leave blank and it will be called “${workspaceDefault}”.`
          }
          label={FIELD_LABELS.organizationName}
          name="organizationName"
          onChange={setOrganizationName}
          optional
          value={organizationName}
        />

        <button className={styles.submit} disabled={pending || rateLimited} type="submit">
          {pending ? "Creating your account…" : "Create account"}
        </button>
      </form>

      <p className={styles.footer}>
        Already have an account? <Link href={signInHref(returnTo)}>Sign in</Link>.
      </p>
    </>
  );
}
