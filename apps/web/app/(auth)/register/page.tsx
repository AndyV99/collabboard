import type { Metadata } from "next";
import { redirect } from "next/navigation";

import { RegisterForm } from "@/components/auth/register-form";
import styles from "@/components/auth/auth.module.css";
import { RETURN_TO_PARAM, safeReturnPath } from "@/lib/auth/routes";
import { getRenderSession } from "@/lib/session/server";

export const metadata: Metadata = {
  title: "Create an account · CollabBoard",
};

type SearchParams = Record<string, string | string[] | undefined>;

function firstValue(value: string | string[] | undefined): string | null {
  if (Array.isArray(value)) {
    return value[0] ?? null;
  }

  return value ?? null;
}

/**
 * The sign-up screen.
 *
 * Same shape as the sign-in page: dynamic, session checked before render, and
 * the return destination validated here so the form never sees an unchecked one.
 */
export default async function RegisterPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const returnTo = safeReturnPath(firstValue((await searchParams)[RETURN_TO_PARAM]));

  if ((await getRenderSession()) !== null) {
    redirect(returnTo);
  }

  return (
    <>
      <header>
        <h1 className={styles.heading}>Create your CollabBoard account</h1>
        <p className={styles.lede}>
          This also creates the workspace your projects and boards live in. You
          will own it.
        </p>
      </header>

      <RegisterForm returnTo={returnTo} />
    </>
  );
}
