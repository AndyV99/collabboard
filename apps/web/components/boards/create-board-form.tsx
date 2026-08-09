"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { api } from "@/lib/api/browser";
import { createBoard } from "@/lib/api/endpoints";
import { describeWriteFailure } from "@/lib/workspace/outcomes";
import { boardHref } from "@/lib/workspace/routes";
import { MAX_NAME_CODE_POINTS, validateName } from "@/lib/workspace/rules";
import { FormMessage, TextField } from "@/components/workspace/fields";
import styles from "@/components/workspace/workspace.module.css";

/**
 * Creates a board inside a project and goes to it.
 *
 * One field, so there is no error summary: a summary that can only ever list one
 * item is a second copy of the field's own message and one more thing for a
 * screen reader to read. The failure block is still `role="alert"` and still
 * takes focus, because that is the part the user cannot see coming.
 *
 * A 404 from `POST /projects/:id/boards` means the project id named nothing this
 * tenant can see — the insert is an `INSERT ... SELECT` over `projects`, so
 * another tenant's project produces no row rather than a foreign-key violation.
 * {@link describeWriteFailure} says "that no longer exists… reload", which is
 * the correct instruction for both the archived-elsewhere case and the
 * wrong-workspace one, and does not claim to know which.
 */
export function CreateBoardForm({
  projectId,
  autoFocus = false,
}: {
  projectId: string;
  autoFocus?: boolean;
}) {
  const router = useRouter();
  const nameId = `${useId()}-name`;

  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const messageRef = useRef<HTMLDivElement | null>(null);
  const nameRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      messageRef.current?.focus();
    }
  }, [attempt]);

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

    const nextNameError = validateName(name, "Board");

    setNameError(nextNameError);

    if (nextNameError !== undefined) {
      setFailure(null);

      return;
    }

    setPending(true);
    setFailure(null);

    const result = await api(createBoard(projectId, { name: name.trim() }));

    if (!result.ok) {
      setPending(false);
      setFailure(describeWriteFailure(result.error, "create the board"));
      setAttempt((count) => count + 1);

      return;
    }

    // Left pending through the navigation, for the same reason as the project
    // form: a re-enabled button during a route change is a second board.
    router.push(boardHref(projectId, result.data.id));
    router.refresh();
  }

  return (
    <form className={styles.form} noValidate onSubmit={handleSubmit}>
      {failure !== null && (
        <FormMessage messageRef={messageRef} title="Could not create the board">
          <p>{failure}</p>
        </FormMessage>
      )}

      <TextField
        disabled={pending}
        error={nameError}
        hint="One board per workflow — a sprint, a launch, a hiring pipeline."
        id={nameId}
        inputRef={nameRef}
        label="Board name"
        maxLength={MAX_NAME_CODE_POINTS}
        onChange={setName}
        value={name}
      />

      <button className={styles.submit} disabled={pending} type="submit">
        {pending ? "Creating…" : "Create board"}
      </button>
    </form>
  );
}
