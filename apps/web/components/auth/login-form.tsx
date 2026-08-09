"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useRef, useState } from "react";

import type { AuthFailure } from "@/lib/auth/outcomes";
import { describeLoginFailure } from "@/lib/auth/outcomes";
import {
  type FieldErrors,
  type LoginField,
  LOGIN_FIELD_ORDER,
  hasErrors,
  normalizeEmail,
  validateLogin,
} from "@/lib/auth/rules";
import { signUpHref } from "@/lib/auth/routes";
import { LOGIN_PATH, submitAuth } from "@/lib/auth/submit";
import { FieldErrorList, FormAlert } from "./form-alert";
import { TextField } from "./text-field";
import { WorkspaceRecovery } from "./workspace-recovery";
import styles from "./auth.module.css";

const FIELD_LABELS: Record<LoginField, string> = {
  email: "Email address",
  password: "Password",
};

/**
 * The sign-in form.
 *
 * # It cannot tell you whether an address is registered, and neither can this
 *
 * `apps/api` answers an unknown address and a wrong password identically, and
 * spends one argon2id derivation either way so the two take the same time
 * (`Service.Login` and `absentAccountParams`). This form does nothing to give
 * that back: one message for both, never a per-field "we don't know that
 * address", and the sign-up link below the form is unconditional — a link that
 * appeared only when the address was unknown would be the same disclosure
 * wearing a friendlier hat.
 *
 * It also does not check the *shape* of the password before submitting, only
 * that one was typed. "That is too short to be one of ours" is a statement about
 * a stored value, and it is useless besides: an account made under an older rule
 * still has to be able to sign in.
 *
 * # One outcome is an action, not a sentence
 *
 * A bare 403 means the account exists, the password was right, and it belongs to
 * no organization — a registration that broke between its two transactions. That
 * used to end in "contact support", which stopped being true when #34 shipped
 * `POST /api/v1/organizations`. It is now the only `AuthFailureKind` this form
 * branches on: it renders {@link WorkspaceRecovery}, which creates the missing
 * workspace and signs the user in. Every other kind renders `failure.message` in
 * one alert, exactly as before.
 *
 * The password this form is holding is what makes that possible, and
 * `workspace-recovery.tsx` documents that decision rather than this file,
 * because that is where it is acted on.
 *
 * # Why the form is `noValidate`
 *
 * The browser's `type="email"` constraint is stricter than the API's, which asks
 * only for an `@` with something in front of it. A browser refusing a form the
 * server would have accepted is a rule with no authority behind it, so
 * constraint validation is off and `lib/auth/rules.ts` — which mirrors the Go
 * checks exactly — decides. `type="email"` stays for the on-screen keyboard and
 * for autofill.
 */
export function LoginForm({ returnTo }: { returnTo: string }) {
  const router = useRouter();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [fieldErrors, setFieldErrors] = useState<FieldErrors<LoginField>>({});
  const [failure, setFailure] = useState<AuthFailure | null>(null);
  const [pending, setPending] = useState(false);
  const [blockedUntil, setBlockedUntil] = useState<number | null>(null);

  // Bumped on every rejected submit. Focus moves in an effect keyed on this
  // rather than in the handler, because the alert does not exist yet when the
  // handler runs — and keying on a counter rather than on the message means two
  // identical failures in a row still move focus.
  const [attempt, setAttempt] = useState(0);
  const alertRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      alertRef.current?.focus();
    }
  }, [attempt]);

  // Re-enables the button when a 429's Retry-After has elapsed. Respecting the
  // header is not just good manners: every refused attempt still counts against
  // the budget, so retrying early lengthens the block.
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

  function reject(errors: FieldErrors<LoginField>, nextFailure: AuthFailure | null) {
    setFieldErrors(errors);
    setFailure(nextFailure);
    setAttempt((count) => count + 1);
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending || rateLimited) {
      return;
    }

    const errors = validateLogin({ email, password });

    if (hasErrors(errors)) {
      reject(errors, null);

      return;
    }

    setPending(true);
    setFieldErrors({});
    setFailure(null);

    const result = await submitAuth(
      LOGIN_PATH,
      // Normalised here as well as on the API, so the address the per-account
      // rate limiter counts against does not depend on how it was capitalised.
      { email: normalizeEmail(email), password },
      describeLoginFailure,
    );

    if (!result.ok) {
      setPending(false);
      reject({}, result.failure);

      if (result.failure.retryAfterSeconds !== undefined) {
        setBlockedUntil(Date.now() + result.failure.retryAfterSeconds * 1000);
      }

      return;
    }

    // Deliberately left pending. The cookies are set and this component is
    // about to be replaced; re-enabling the button first would offer a second
    // sign-in during the navigation.
    router.replace(returnTo);
    router.refresh();
  }

  const listedErrors = LOGIN_FIELD_ORDER.filter(
    (field) => fieldErrors[field] !== undefined,
  ).map((field) => ({
    field,
    message: `${FIELD_LABELS[field]}: ${fieldErrors[field]}`,
  }));

  return (
    <>
      {listedErrors.length > 0 && (
        <FormAlert alertRef={alertRef} title="Check the form">
          <FieldErrorList errors={listedErrors} />
        </FormAlert>
      )}

      {/*
        The one kind that gets more than a sentence. Every other failure renders
        exactly as it did before — one alert, `failure.message`, no branch — and
        that is deliberate: `no_organization` is the only outcome on this screen
        with an action the user can take, so it is the only one that earns a
        different shape. See `workspace-recovery.tsx`.
      */}
      {listedErrors.length === 0 &&
        failure !== null &&
        failure.kind === "no_organization" && (
          <WorkspaceRecovery
            alertRef={alertRef}
            // A getter, not the password itself. The component cannot hold what
            // it is only handed inside a callback, and the value it reads is
            // whatever is in the form at the moment of the click.
            credentials={() => ({ email, password })}
            message={failure.message}
            onRateLimited={(seconds) => setBlockedUntil(Date.now() + seconds * 1000)}
            onSignedIn={() => {
              router.replace(returnTo);
              router.refresh();
            }}
          />
        )}

      {listedErrors.length === 0 &&
        failure !== null &&
        failure.kind !== "no_organization" && (
          <FormAlert alertRef={alertRef} title="Could not sign you in">
            <p>{failure.message}</p>
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
          autoComplete="current-password"
          disabled={pending}
          error={fieldErrors.password}
          label={FIELD_LABELS.password}
          name="password"
          onChange={setPassword}
          revealable
          type="password"
          value={password}
        />

        <button className={styles.submit} disabled={pending || rateLimited} type="submit">
          {pending ? "Signing in…" : "Sign in"}
        </button>
      </form>

      <p className={styles.footer}>
        New to CollabBoard? <Link href={signUpHref(returnTo)}>Create an account</Link>.
      </p>
    </>
  );
}
