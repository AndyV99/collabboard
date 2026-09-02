"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { api } from "@/lib/api/browser";
import { updateProject } from "@/lib/api/endpoints";
import type { Project } from "@/lib/api/types";
import { describeWriteFailure } from "@/lib/workspace/outcomes";
import {
  MAX_DESCRIPTION_CODE_POINTS,
  MAX_NAME_CODE_POINTS,
  maxLengthFor,
  projectChanged,
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
 * Renames a project, and edits its description.
 *
 * # Why it sends both fields and not just the changed one
 *
 * `PATCH /projects/:id` distinguishes "not mentioned" from "set to empty" —
 * `patchProjectRequest` takes `*string` and `UpdateProject` coalesces a nil onto
 * the existing column — so sending only the name would be the *correct* way to
 * rename without touching the description. This form edits both at once, so it
 * states both, and the difference matters in one direction: clearing the
 * description has to send `""` rather than omitting the key, or the clear would
 * silently do nothing.
 *
 * A submit that changes neither is refused before it is sent, because the API
 * answers a body mentioning neither field with a 400 — and "at least one of name
 * or description is required" is a confusing thing to be told for pressing Save
 * on a form you did not edit. {@link projectChanged} is that check.
 *
 * # Why there is no optimistic update
 *
 * The server's answer is the project, and the page is server-rendered, so
 * `router.refresh()` shows the stored value rather than the typed one. On a
 * rename that succeeds the two are identical; on a rename the API trimmed or
 * rejected they are not, and the stored one is the truth.
 */
export function RenameProjectForm({ project }: { project: Project }) {
  const router = useRouter();
  const fieldId = useId();

  const nameId = `${fieldId}-name`;
  const descriptionId = `${fieldId}-description`;

  const [name, setName] = useState(project.name);
  const [description, setDescription] = useState(project.description);
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [descriptionError, setDescriptionError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [pending, setPending] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const messageRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      messageRef.current?.focus();
    }
  }, [attempt]);

  const changed = projectChanged(project, { name, description });

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending || !changed) {
      return;
    }

    const nextNameError = validateName(name, "Project");
    const nextDescriptionError = validateDescription(description);

    setNameError(nextNameError);
    setDescriptionError(nextDescriptionError);
    setSaved(false);

    if (nextNameError !== undefined || nextDescriptionError !== undefined) {
      setFailure(null);
      setAttempt((count) => count + 1);

      return;
    }

    setPending(true);
    setFailure(null);

    const result = await api(
      updateProject(project.id, {
        name: name.trim(),
        description: description.trim(),
      }),
    );

    setPending(false);

    if (!result.ok) {
      setFailure(describeWriteFailure(result.error, "rename this project"));
      setAttempt((count) => count + 1);

      return;
    }

    // The fields are re-seeded from the response rather than from what was
    // typed, so the form agrees with the server about what is stored — the API
    // trims, and a form still holding the untrimmed string would offer to
    // "save" a change that is not one.
    setName(result.data.name);
    setDescription(result.data.description);
    setSaved(true);
    setAttempt((count) => count + 1);

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
        <FormMessage messageRef={messageRef} title="Could not save the changes">
          <p>{failure}</p>
        </FormMessage>
      )}

      {listed.length === 0 && failure === null && saved && (
        <FormMessage messageRef={messageRef} title="Saved" tone="notice">
          <p>Everyone in this workspace sees the new details.</p>
        </FormMessage>
      )}

      <TextField
        disabled={pending}
        error={nameError}
        id={nameId}
        label="Project name"
        maxLength={maxLengthFor(MAX_NAME_CODE_POINTS)}
        onChange={setName}
        value={name}
      />

      <TextAreaField
        disabled={pending}
        error={descriptionError}
        hint="Leave it empty to remove the description."
        id={descriptionId}
        label="Description"
        maxLength={maxLengthFor(MAX_DESCRIPTION_CODE_POINTS)}
        onChange={setDescription}
        optional
        value={description}
      />

      <button className={styles.submit} disabled={pending || !changed} type="submit">
        {pending ? "Saving…" : "Save changes"}
      </button>
    </form>
  );
}
