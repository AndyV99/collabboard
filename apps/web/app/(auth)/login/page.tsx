import type { Metadata } from "next";
import { redirect } from "next/navigation";

import { LoginForm } from "@/components/auth/login-form";
import styles from "@/components/auth/auth.module.css";
import { RETURN_TO_PARAM, safeReturnPath } from "@/lib/auth/routes";
import { getRenderSession } from "@/lib/session/server";

export const metadata: Metadata = {
  title: "Sign in · CollabBoard",
};

type SearchParams = Record<string, string | string[] | undefined>;

/** The first value of a repeated query parameter, which is what a browser sends. */
function firstValue(value: string | string[] | undefined): string | null {
  if (Array.isArray(value)) {
    return value[0] ?? null;
  }

  return value ?? null;
}

/**
 * The sign-in screen.
 *
 * A Server Component, so the "are you already signed in" question is answered
 * before anything renders and there is no flash of a form the visitor did not
 * need. `searchParams` is a promise in this version of Next; awaiting it is also
 * what marks the page dynamic, which it must be — a prerendered sign-in page
 * would be one with somebody else's session baked into it.
 *
 * The destination is run through `safeReturnPath` here rather than being trusted
 * downstream, so the form is handed a path this app has already agreed to
 * navigate to. See `lib/auth/routes.ts` for what that check refuses.
 */
export default async function LoginPage({
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
        <h1 className={styles.heading}>Sign in to CollabBoard</h1>
        <p className={styles.lede}>
          Your boards, and everyone else&rsquo;s in your workspace.
        </p>
      </header>

      <LoginForm returnTo={returnTo} />
    </>
  );
}
