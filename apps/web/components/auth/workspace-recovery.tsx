"use client";

import { type FormEvent, type RefObject, useEffect, useRef, useState } from "react";

import type { AuthFailure } from "@/lib/auth/outcomes";
import {
  describeFirstOrganizationFailure,
  describeLoginFailure,
} from "@/lib/auth/outcomes";
import {
  MAX_ORGANIZATION_NAME_CODE_POINTS,
  codePointLength,
  hasErrors,
  normalizeEmail,
  validateLogin,
} from "@/lib/auth/rules";
import { FIRST_ORGANIZATION_PATH, LOGIN_PATH, submitAuth } from "@/lib/auth/submit";
import { FieldErrorList, FormAlert } from "./form-alert";
import { TextField } from "./text-field";
import styles from "./auth.module.css";

/**
 * The way out of a half-completed registration, offered on the sign-in screen.
 *
 * Rendered by `login-form.tsx` in place of the plain sentence, and only for
 * `failure.kind === "no_organization"`. Everything else on that screen still
 * renders `failure.message` and nothing else.
 *
 * # The password, which is the decision this component exists to make
 *
 * `POST /api/v1/organizations` takes an email and a password rather than a
 * token, and ADR 0009 explains at length why that is structural: an account with
 * zero memberships cannot hold a token, so login refuses to issue one, the
 * issuer refuses a nil tenant, and the verifier refuses a zero `org` claim. The
 * password is the only durable credential such an account has.
 *
 * The user typed one into the form a moment ago. Offering recovery without
 * asking for it again means the form holds it across the transition, so:
 *
 * **Where it lives.** Exactly where it already lived. `login-form.tsx` binds the
 * password input to `useState`, so from the first keystroke until the component
 * unmounts, React holds that string — that is what a controlled input *is*, and
 * it was true before this component existed. Reading it again on a second button
 * does not extend that lifetime by a millisecond. The risk that would have been
 * new is a *copy* with a different lifetime, so there is not one:
 * {@link WorkspaceRecoveryProps.credentials} is a function, not a value. This
 * component never receives the password as a prop, never puts it in state, never
 * puts it in a ref, and never closes over it outside the submit handler. It calls
 * the getter, sends what it gets, and lets it go. That signature is the point:
 * it is not possible to stash a password you are only handed inside a callback.
 *
 * **Where it never goes.** `localStorage`, `sessionStorage`, cookies, the URL,
 * a query param, or a log line. It travels the same route the login form's
 * password travels and no other: a JSON body, over same-origin `fetch`, to a
 * Route Handler that forwards it server-side. `readJsonBody` refuses to echo a
 * body it could not parse for exactly this reason, and neither the handler's log
 * lines nor the API's name the address, let alone the credential.
 *
 * **When it goes away.** When the component unmounts. On the happy path that is
 * immediate — the follow-up sign-in navigates away and the whole form goes with
 * it. On a failure it stays, because the alternative is discarding what the user
 * typed, which is the one thing the issue calls out as unacceptable. No timer
 * clears it: a field that empties itself while someone reads the error is a
 * worse product and not a meaningfully safer one, since the same string is
 * sitting in the password box either way.
 *
 * The honest alternative was re-prompting. It was rejected because it buys
 * nothing: the password would be typed into a second input in the same document
 * and held in the same kind of state, so the memory exposure is identical and
 * the only difference is that the user types it twice.
 *
 * # Why it reads the credentials live rather than snapshotting them
 *
 * The getter returns whatever is in the form *now*. A snapshot taken at the 403
 * would be a second copy, and it would also lie: if the user edits the address
 * and then clicks this button, the workspace they get should belong to the
 * account whose password actually verifies, which is what the API decides and
 * what a live read asks about. An edited password simply produces a 401, which
 * is the truth.
 *
 * # The workspace name is asked for, optionally
 *
 * `POST /organizations` defaults an absent name to `"<display name>'s
 * workspace"`, exactly as registration does, so not asking would have been
 * defensible and one field lighter. It is asked anyway because there is no
 * rename: `/organizations` is the only organizations route on the API, so a
 * workspace named by the default is named that permanently. This user very
 * likely *did* choose a name during the sign-up that broke, and it was lost with
 * the transaction. Leaving the box blank still costs one click.
 *
 * The length cap mirrors `validateRegistration` rather than the API, which
 * bounds this field nowhere — that is issue #67, and the cap here is the same
 * client-side mitigation the sign-up form already applies, deliberately not a
 * new rule invented on this screen.
 */
export type WorkspaceRecoveryProps = {
  /**
   * Reads the credentials out of the sign-in form at the moment of the click.
   *
   * A function rather than two strings so that this component is structurally
   * incapable of holding a password. See the module comment.
   */
  credentials: () => { email: string; password: string };
  /** The sentence explaining the state — `NO_ORGANIZATION`, from the parent. */
  message: string;
  /** The sign-in form's alert element, so focus management stays in one place. */
  alertRef: RefObject<HTMLDivElement | null>;
  /** Called when this route's 429 arrives, so the shared budget is respected. */
  onRateLimited: (seconds: number) => void;
  /** Navigates on success. Supplied by the parent, which owns the router. */
  onSignedIn: () => void;
};

/** What is left to do once a workspace is known to exist. */
type Resolution = "created" | "exists";

export function WorkspaceRecovery({
  credentials,
  message,
  alertRef,
  onRateLimited,
  onSignedIn,
}: WorkspaceRecoveryProps) {
  const [organizationName, setOrganizationName] = useState("");
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<AuthFailure | null>(null);
  const [resolution, setResolution] = useState<Resolution | null>(null);
  const [pending, setPending] = useState(false);
  const [blockedUntil, setBlockedUntil] = useState<number | null>(null);

  // Same shape as the sign-in form's: focus moves in an effect keyed on a
  // counter, because the alert's content does not exist yet when the handler
  // runs and two identical failures in a row must still move focus.
  const [attempt, setAttempt] = useState(0);
  const firstRender = useRef(true);

  useEffect(() => {
    // The parent already focused the alert for the 403 that rendered this
    // component. Focusing again on mount would be a second, redundant move.
    if (firstRender.current) {
      firstRender.current = false;

      return;
    }

    alertRef.current?.focus();
  }, [attempt, alertRef]);

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

  function announce(next: AuthFailure | null, resolved: Resolution | null) {
    setFailure(next);
    setResolution(resolved);
    setAttempt((count) => count + 1);

    if (next?.retryAfterSeconds !== undefined) {
      setBlockedUntil(Date.now() + next.retryAfterSeconds * 1000);
      // The sign-in button spends the same budget, so it has to stop too.
      // Retrying either one early only lengthens the block.
      onRateLimited(next.retryAfterSeconds);
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending || rateLimited) {
      return;
    }

    const trimmedName = organizationName.trim();

    if (codePointLength(trimmedName) > MAX_ORGANIZATION_NAME_CODE_POINTS) {
      setNameError(
        `Workspace names can be at most ${MAX_ORGANIZATION_NAME_CODE_POINTS} characters.`,
      );
      setFailure(null);
      setAttempt((count) => count + 1);

      return;
    }

    // Read once, here, and never stored. `email` is normalised the way the API's
    // rate limiter normalises it, so the budget this call is charged against is
    // the same one the sign-in attempt just spent from.
    const { email, password } = credentials();

    // Both boxes are still editable while this button is on screen, so either
    // can be empty by the time it is pressed. That must not become a request:
    // `CreateFirstOrganization` charges the shared sign-in budget *before* it
    // verifies the credential, so an empty password would spend a real slot on
    // something that could not have succeeded. `login-form.tsx` runs exactly
    // this check before its own submit; pressing this button bypasses that
    // form's handler, so the check has to be repeated rather than inherited.
    if (hasErrors(validateLogin({ email, password }))) {
      setNameError(undefined);
      announce(
        {
          kind: "invalid_input",
          message:
            "Your email address and password are both needed to create the workspace. Check them in the form below and try again.",
        },
        null,
      );

      return;
    }

    setPending(true);
    setNameError(undefined);
    setFailure(null);

    const normalizedEmail = normalizeEmail(email);

    const created = await submitAuth(
      FIRST_ORGANIZATION_PATH,
      {
        email: normalizedEmail,
        password,
        // Omitted rather than sent empty, so the API applies its own default
        // instead of this app duplicating it — the same call the sign-up form
        // makes, reaching the same `workspaceName` on the other side.
        ...(trimmedName === "" ? {} : { organization_name: trimmedName }),
      },
      describeFirstOrganizationFailure,
    );

    if (!created.ok) {
      setPending(false);

      // A 409 is not a failure to report. It means the workspace exists —
      // another tab, or a second click that raced — so the situation resolved
      // itself and the only step left is the one the form below already does.
      announce(
        created.failure,
        created.failure.kind === "organization_exists" ? "exists" : null,
      );

      return;
    }

    // The workspace exists now. `POST /organizations` answers 201 with no
    // tokens, deliberately (ADR 0009), so signing in is a separate call — the
    // same one the sign-up form makes after registering, for the same reason.
    const signedIn = await submitAuth(
      LOGIN_PATH,
      { email: normalizedEmail, password },
      describeLoginFailure,
    );

    if (!signedIn.ok) {
      setPending(false);
      // The workspace was created. Whatever went wrong with the sign-in, the
      // user must not be left thinking the whole thing failed and try again —
      // the retry would be a 409. A 429 is the likely one here, because this
      // call and the one above share a budget with the sign-in that failed.
      announce(signedIn.failure, "created");

      return;
    }

    // Left pending, as the sign-in form does: the cookies are set and this tree
    // is about to be replaced.
    onSignedIn();
  }

  return (
    <>
      {nameError !== undefined && (
        <FormAlert alertRef={alertRef} title="Check the form">
          <FieldErrorList
            errors={[{ field: "organizationName", message: `Workspace name: ${nameError}` }]}
          />
        </FormAlert>
      )}

      {nameError === undefined && resolution === "created" && failure !== null && (
        <FormAlert alertRef={alertRef} title="Your workspace was created" tone="notice">
          <p>
            {failure.message} Your workspace is ready — sign in below to open it,
            and do not create another.
          </p>
        </FormAlert>
      )}

      {nameError === undefined && resolution === "exists" && failure !== null && (
        <FormAlert
          alertRef={alertRef}
          title="This account already has a workspace"
          tone="notice"
        >
          <p>{failure.message}</p>
        </FormAlert>
      )}

      {nameError === undefined && resolution === null && (
        <FormAlert
          alertRef={alertRef}
          title={failure === null ? "Your account has no workspace" : "Could not create your workspace"}
        >
          <p>{failure === null ? message : failure.message}</p>
        </FormAlert>
      )}

      {resolution === null && (
        <form className={styles.form} noValidate onSubmit={handleSubmit}>
          <TextField
            autoComplete="organization"
            disabled={pending}
            error={nameError}
            hint="Leave this blank and it will be named after you. It cannot be renamed later."
            label="Workspace name"
            name="organizationName"
            // Clears the length error as the name is shortened, rather than
            // leaving `aria-invalid` and the summary contradicting the field
            // until the next submit.
            onChange={(next) => {
              setOrganizationName(next);
              setNameError(undefined);
            }}
            optional
            value={organizationName}
          />

          <button
            className={styles.submit}
            disabled={pending || rateLimited}
            type="submit"
          >
            {pending ? "Creating your workspace…" : "Create workspace and sign in"}
          </button>
        </form>
      )}
    </>
  );
}
