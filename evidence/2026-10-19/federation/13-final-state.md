# Final state, 2026-10-19T03:22:35Z

## kw replication rules
watteel/oci-hosted  in_sync  adopted=true  lag=0
watteel/npm         in_sync  adopted=true  lag=0
watteel/sccache     in_sync  adopted=true  lag=0
watteel/pypi        in_sync  adopted=true  lag=0
watteel/nuget       in_sync  adopted=true  lag=0
watteel/go          in_sync  adopted=true  lag=0
watteel/fed-qual    in_sync  adopted=true  lag=0
watteel/helm        in_sync  adopted=true  lag=0
watteel/maven       in_sync  adopted=true  lag=0
watteel/cargo       in_sync  adopted=true  lag=0

## kw jobs (checkpoint = remote head means caught up)
oke-cargo       federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-go          federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-helm        federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-maven       federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-npm         federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-nuget       federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-oci-hosted  federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-pypi        federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0
oke-sccache     federation  checkpoint=9490  remote_head=9490  behind=0  reseeds=0  failures=0

## health
oke: ok alerts=0 epoch=1 policy=7 topology=31
kw:  ok alerts=0 epoch=1 policy=7 topology=31

## left in place
feeds fed-route (OKE only), fed-qual (OKE home, replicated to kw) and fed-oci (OKE only) with their keys and the fed-qual rule; empty federation policy (generation 7: the qualification service account is disabled, its tokens revoked); NetworkPolicy qual-partition deleted; kw Service ant-ant-federation-lan and OKE hostAliases kept; OKE federation LoadBalancer maximum 100 Mbit/s.
