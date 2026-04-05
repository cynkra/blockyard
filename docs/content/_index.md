---
title: Blockyard
type: docs
---

# Blockyard

Deploy and run Shiny applications in isolated containers with zero configuration.

<div class="book-columns flex">
<div>

[Get Started]({{< relref "/docs/getting-started/overview" >}})

</div>
<div>

[CLI Reference]({{< relref "/docs/reference/cli" >}})

</div>
</div>

## Why Blockyard?

{{< cardgrid >}}

{{< card title="Container Isolation" >}}
Each Shiny app runs in its own container with network isolation,
read-only filesystems, and dropped capabilities.
{{< /card >}}

{{< card title="Automatic Dependencies" >}}
Upload a bundle and Blockyard resolves and installs
R package dependencies automatically via pak.
{{< /card >}}

{{< card title="Cold Start" >}}
Workers are spawned on-demand when a user visits your app.
No idle containers wasting resources.
{{< /card >}}

{{< card title="Authentication & RBAC" >}}
OIDC login, role-based access control, and per-app ACLs.
Optional OpenBao integration for credential management.
{{< /card >}}

{{< card title="CLI & REST API" >}}
Deploy from your terminal with `by deploy` or script with
the REST API. One config file. One binary.
{{< /card >}}

{{< /cardgrid >}}

## Documentation

{{< cardgrid >}}

{{< linkcard title="Getting Started" description="Installation and first deployment." href="/docs/getting-started/overview/" >}}

{{< linkcard title="Guides" description="Configuration, authorization, and operations." href="/docs/guides/deploying/" >}}

{{< linkcard title="Reference" description="CLI commands, REST API, and configuration file." href="/docs/reference/cli/" >}}

{{< /cardgrid >}}
