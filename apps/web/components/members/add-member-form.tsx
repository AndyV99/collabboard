"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { api } from "@/lib/api/browser";
import { addMember } from "@/lib/api/endpoints";
import { type AddMemberFailure, describeAddMemberFailure } from "@/lib/workspace/outcomes";
import { ROLE_MEMBER, describeRole } from "@/lib/workspace/roles";
import {
  MAX_EMAIL_BYTES,
  maxLengthForBytes,
  validateMemberEmail,
} from "@/lib/workspace/rules";
import { FormMessage, SelectField, TextField } from "@/components/workspace/fields";
import styles from "@/components/workspace/workspace.module.css";

/**
 * Adds an existing account to the workspace.
 *
 * # This form is never rendered for someone who may not use it
 *
 * The page decides, from the caller's row in `GET /members` rather than from the
 * token's role claim — see `lib/workspace/roles.ts` for why those two can
 * disagree and why the row is the one the API will check. So a `member` does not
 * see a disabled button or a form that 403s; they see a sentence explaining who
 * can add people.
 *
 * `roles` is the set that role may grant, and it comes from the same place.
 * An owner sees `member` and `admin`; an admin sees `member` only; nobody is
 * offered `owner`, because `validateAddMember` refuses it as a property of the
 * endpoint rather than of the caller. The form cannot offer a choice the server
 * will reject.
 *
 * # What "add" means here, and what the copy has to be honest about
 *
 * This is a direct add, not an invitation: ADR 0008 records why (an accept step
 * needs a pre-tenant *write*, and delivering an invitation needs a mailer this
 * service does not have). Two consequences the user can see, so both are said
 * out loud rather than discovered:
 *
 * - the person is in the workspace **immediately**, with no acceptance;
 * - an address with no CollabBoard account **cannot** be added at all — it is a
 *   404, because this path never creates a user.
 */
export function AddMemberForm({ roles }: { roles: readonly string[] }) {
  const router = useRouter();
  const fieldId = useId();

  const emailId = `${fieldId}-email`;
  const roleId = `${fieldId}-role`;

  const [email, setEmail] = useState("");
  const [role, setRole] = useState(roles[0] ?? ROLE_MEMBER);
  const [emailError, setEmailError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<AddMemberFailure | null>(null);
  const [added, setAdded] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const messageRef = useRef<HTMLDivElement | null>(null);
  const emailRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      messageRef.current?.focus();
    }
  }, [attempt]);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const nextEmailError = validateMemberEmail(email);

    setEmailError(nextEmailError);
    setAdded(null);

    if (nextEmailError !== undefined) {
      setFailure(null);

      return;
    }

    setPending(true);
    setFailure(null);

    // Normalised here as well as on the API. `NormalizeEmail` trims and
    // lower-cases, and matching it means the address in the success message is
    // the address that was stored rather than the capitalisation that was typed.
    const normalized = email.trim().toLowerCase();

    const result = await api(addMember({ email: normalized, role }));

    setPending(false);

    if (!result.ok) {
      setFailure(describeAddMemberFailure(result.error));
      setAttempt((count) => count + 1);

      return;
    }

    setEmail("");
    setAdded(result.data.email);
    setAttempt((count) => count + 1);

    // The list on this page is server-rendered, so the new member appears
    // through a re-render rather than by being spliced into client state. One
    // source of truth, and the row that appears is the row the API stored —
    // display name included, which the 201 deliberately does not carry.
    router.refresh();
  }

  const roleOptions = roles.map((value) => ({
    value,
    label: `${value.charAt(0).toUpperCase()}${value.slice(1)} — ${describeRole(value)}`,
  }));

  return (
    <form className={styles.form} noValidate onSubmit={handleSubmit}>
      {failure !== null && (
        <FormMessage messageRef={messageRef} title="Could not add them">
          <p>{failure.message}</p>
        </FormMessage>
      )}

      {failure === null && added !== null && (
        <FormMessage messageRef={messageRef} title="Added to the workspace" tone="notice">
          <p>
            {added} is in this workspace now, with access to every project in it. They
            were not sent anything — tell them yourself.
          </p>
        </FormMessage>
      )}

      <TextField
        autoComplete="off"
        disabled={pending}
        error={emailError}
        hint="They need a CollabBoard account already. Adding an address nobody has signed up with is not possible — this never creates an account."
        id={emailId}
        inputRef={emailRef}
        label="Email address"
        maxLength={maxLengthForBytes(MAX_EMAIL_BYTES)}
        onChange={setEmail}
        type="email"
        value={email}
      />

      {roleOptions.length > 1 ? (
        <SelectField
          disabled={pending}
          id={roleId}
          label="Role"
          onChange={setRole}
          options={roleOptions}
          value={role}
        />
      ) : (
        <p className={styles.hint}>
          They will join as a <strong>member</strong>: {describeRole(ROLE_MEMBER)} Only
          an owner can add an admin.
        </p>
      )}

      <button className={styles.submit} disabled={pending} type="submit">
        {pending ? "Adding…" : "Add to workspace"}
      </button>
    </form>
  );
}
