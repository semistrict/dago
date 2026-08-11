# Upstream baseline

This example was copied from and modified from
[`boldsoftware/shelley`](https://github.com/boldsoftware/shelley) at commit
`1d4cbe79c6be45cc0105d46819cb54844f98eddd` (2026-08-08).

The example preserves Shelley's Apache-2.0 license while replacing its agent
runtime with dago. Tests that exclusively exercised reusable agent behavior now
live with the corresponding dago packages; the tests remaining here exercise
the application and its integration with dago.
