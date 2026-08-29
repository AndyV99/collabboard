import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { HealthPanel } from "@/components/health-panel";
import type { HealthProbe } from "@/lib/health";

const URL = "http://localhost:8080/healthz";

describe("HealthPanel", () => {
  it("renders each dependency when the API is healthy", () => {
    const probe: HealthProbe = {
      outcome: "reachable",
      url: URL,
      httpStatus: 200,
      health: {
        status: "ok",
        components: { postgres: { status: "ok" }, redis: { status: "ok" } },
      },
    };

    render(<HealthPanel probe={probe} />);

    expect(screen.getByText("Healthy")).toBeInTheDocument();
    expect(screen.getByText("postgres")).toBeInTheDocument();
    expect(screen.getByText("redis")).toBeInTheDocument();
    expect(screen.getByText(URL)).toBeInTheDocument();
    expect(screen.getByText("HTTP 200")).toBeInTheDocument();
  });

  it("surfaces the failing dependency and its error on a 503", () => {
    const probe: HealthProbe = {
      outcome: "reachable",
      url: URL,
      httpStatus: 503,
      health: {
        status: "unavailable",
        components: {
          postgres: { status: "ok" },
          redis: { status: "unavailable", error: "dial tcp 127.0.0.1:6379" },
        },
      },
    };

    render(<HealthPanel probe={probe} />);

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(screen.getByText("HTTP 503")).toBeInTheDocument();
    expect(screen.getByText("dial tcp 127.0.0.1:6379")).toBeInTheDocument();
    // The healthy dependency is still reported, so a partial outage is
    // distinguishable from a total one at a glance.
    expect(screen.getByText("postgres")).toBeInTheDocument();
  });

  // The deployed policy from #31: outside development the API omits the driver
  // error entirely, so the panel receives a component with a status and no
  // `error` field. It must still render the outage rather than a blank row --
  // which is what makes redaction acceptable on a page an operator uses.
  //
  // Note there is deliberately no redaction logic on this side. The policy
  // lives in one place, in the API, because two independent redaction rules
  // drift and the quiet direction of that drift is the one that leaks.
  it("renders a degraded dependency that carries no error text", () => {
    const probe: HealthProbe = {
      outcome: "reachable",
      url: URL,
      httpStatus: 503,
      health: {
        status: "unavailable",
        components: {
          postgres: { status: "ok" },
          redis: { status: "unavailable" },
        },
      },
    };

    const { container } = render(<HealthPanel probe={probe} />);

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(screen.getByText("HTTP 503")).toBeInTheDocument();
    // The failing dependency is still named, which is the half an operator
    // needs and the half redaction must not take away.
    expect(screen.getByText("redis")).toBeInTheDocument();
    expect(screen.getByText("postgres")).toBeInTheDocument();

    // And no driver text appears. Asserted against the rendered output rather
    // than the props, so a future change that started echoing something from
    // elsewhere would fail.
    //
    // Deliberately not a "no port numbers" assertion: the panel displays the
    // configured endpoint URL, which contains one. That is client-side
    // configuration the operator set, not topology the API disclosed, and
    // conflating the two would make this test fail for the wrong reason.
    expect(container.textContent).not.toMatch(/dial tcp|refused|timeout|rds\.amazonaws/i);
  });

  it("explains an unreachable API instead of rendering an empty panel", () => {
    const probe: HealthProbe = {
      outcome: "unreachable",
      url: URL,
      error: "fetch failed",
    };

    render(<HealthPanel probe={probe} />);

    expect(screen.getByText("No response")).toBeInTheDocument();
    expect(screen.getByText(/Could not reach the API/)).toBeInTheDocument();
    expect(screen.getByText(/fetch failed/)).toBeInTheDocument();
    // Nothing is known about the dependencies, so none must be claimed healthy.
    expect(screen.queryByText("postgres")).not.toBeInTheDocument();
    expect(screen.queryByText(/^HTTP /)).not.toBeInTheDocument();
  });

  it("explains a response that is not the /healthz shape", () => {
    const probe: HealthProbe = {
      outcome: "malformed",
      url: URL,
      httpStatus: 404,
      error: "response JSON did not match the /healthz schema",
    };

    render(<HealthPanel probe={probe} />);

    expect(screen.getByText("No response")).toBeInTheDocument();
    expect(screen.getByText("HTTP 404")).toBeInTheDocument();
    expect(
      screen.getByText(/did not look like/),
    ).toBeInTheDocument();
  });
});
