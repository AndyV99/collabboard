"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { api } from "@/lib/api/browser";
import { createProject } from "@/lib/api/endpoints";
import { describeWriteFailure } from "@/lib/workspace/outcomes";
import { projectHref } from "@/lib/workspace/routes";
import {
  MAX_DESCRIPTION_CODE_POINTS,
  MAX_NAME_CODE_POINTS,
  validateDescription,
  validateName,
} from "@/lib/workspace/rules";
import {
  FieldErrorList,
  FormMessage,
  TextAreaField,
  TextField,
} from "@/components/workspace/fields";
import styles from "@/components/workspace/workspace.module.css";

/**
 * Creates a project and goes to it.
 *
 * # Why this is a Client Component when nothing else on the page is
 *
 * The list above it is server-rendered, and should be: it has the session
 * already, costs no round trip, and ships no fetching code. A *form* is the case
 * the README calls out as the exception — it needs pending state, per-field
 * errors, and focus management, none of which survive a page reload.
 *
 * It reaches the API through `browserApi`, which posts to this app's own
 * `/api/proxy` and never holds a token. On success it navigates to the new
 * project and calls `router.refresh()`, which is what re-runs the Server
 * Component that will render it. Without the refresh, the router could serve the
 * destination from a cache that predates the project.
 *
 * # Landing in the project rather than back on the list
 *
 * A project with no boards is not a destination anyone wanted; the next thing
 * every user does is create a board. Pushing to the project puts them one step
 * along instead of asking them to find the card they just made.
 */
export function CreateProjectForm({ autoFocus = false }: { autoFocus?: boolean }) {
  const router = useRouter();
  const fieldId = useId();

  const nameId = `${fieldId}-name`;
  const descriptionId = `${fieldId}-description`;

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [descriptionError, setDescriptionError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  // Bumped on every rejected submit, so focus moves even when two identical
  // failures happen in a row. The message does not exist when the handler runs,
  // which is why this is an effect rather than a call inside the handler — the
  // same shape `components/auth/login-form.tsx` uses.
  const [attempt, setAttempt] = useState(0);
  const messageRef = useRef<HTMLDivElement | null>(null);
  const nameRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      messageRef.current?.focus();
    }
  }, [attempt]);

  // Only on the empty state, where this form *is* the page. Stealing focus on a
  // page that already has content would move a keyboard user somewhere they did
  // not ask to be.
  useEffect(() => {
    if (autoFocus) {
      nameRef.current?.focus();
    }
  }, [autoFocus]);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const nextNameError = validateName(name, "Project");
    const nextDescriptionError = validateDescription(description);

    setNameError(nextNameError);
    setDescriptionError(nextDescriptionError);

    if (nextNameError !== undefined || nextDescriptionError !== undefined) {
      setFailure(null);
      setAttempt((count) => count + 1);

      return;
    }

    setPending(true);
    setFailure(null);

    // Trimmed on the way out, because the API trims before it stores and a
    // client that sent the untrimmed value would be validating a different
    // string from the one that ends up in the database.
    const result = await api(
      createProject({ name: name.trim(), description: description.trim() }),
    );

    if (!result.ok) {
      setPending(false);
      setFailure(describeWriteFailure(result.error, "create the project"));
      setAttempt((count) => count + 1);

      return;
    }

    // Deliberately left pending: this component is about to be replaced by the
    // navigation, and re-enabling the button first offers a second submit that
    // would create a duplicate project.
    router.push(projectHref(result.data.id));
    router.refresh();
  }

  const listed = [
    nameError === undefined ? null : { id: nameId, message: `Name: ${nameError}` },
    descriptionError === undefined
      ? null
      : { id: descriptionId, message: `Description: ${descriptionError}` },
  ].filter((entry): entry is { id: string; message: string } => entry !== null);

  return (
    <form className={styles.form} noValidate onSubmit={handleSubmit}>
      {listed.length > 0 && (
        <FormMessage messageRef={messageRef} title="Check the form">
          <FieldErrorList errors={listed} />
        </FormMessage>
      )}

      {listed.length === 0 && failure !== null && (
        <FormMessage messageRef={messageRef} title="Could not create the project">
          <p>{failure}</p>
        </FormMessage>
      )}

      <TextField
        disabled={pending}
        error={nameError}
        hint="Something the team will recognise — a product, a client, a quarter."
        id={nameId}
        inputRef={nameRef}
        label="Project name"
        maxLength={MAX_NAME_CODE_POINTS}
        onChange={setName}
        value={name}
      />

      <TextAreaField
        disabled={pending}
        error={descriptionError}
        id={descriptionId}
        label="Description"
        maxLength={MAX_DESCRIPTION_CODE_POINTS}
        onChange={setDescription}
        optional
        value={description}
      />

      <button className={styles.submit} disabled={pending} type="submit">
        {pending ? "Creating…" : "Create project"}
      </button>
    </form>
  );
}
