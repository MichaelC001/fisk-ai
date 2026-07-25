# Roadmap

This is a rough development roadmap, this isn't set in stone - as we explore this area of development we might find additional
features to add or directions to take.

This would give you guidance on our goals though.

### Expand the RAG system with a GraphRAG layer

While the RAG system we have is sufficient for the kinds of use cases we target today - project documentation etc - it just
is not good enough for all kinds of data we might encounter.

Ideally we use a LLM to extract taxonomies out of source material and then create a Graph from that. RAG to find the starting
point, Graph queries to get the relationships.  This allows us to answer questions like `List all episodes of the TV Series
Criminal Minds that had a female perpetrator`.

See [GraphRAG](https://graphrag.com).

### Integration with Job Systems

We need to integrate with job systems like [Choria Async Jobs](https://github.com/choria-io/asyncjobs) to facilitate
external work arriving into our sphere.

Imagine a Webhook listener that receives a webhook and opens a Job. At this point the webhook is handled, the Agent will
pick it up, do the work and update the record with the outcome

### Finish the A2A system

Today we have tool calling using request-reply but no LLM prompting ability. We have the data types but nothing is wired 
up to them to facilitate true A2A

### Choria Transport

Once the A2A system is mature expose it on the Choria transport with strong Identity, Authentication, Authorization and
Auditing.

### Move API out of `internal` package

A large goal of this project is to create libraries that can be used to build all sorts of AI Agent within the Choria ecosystem.

At the moment the libraries, while being developed and refactored, are all in `internal`. The aim is to make these public.

This would coincide with a end to end POC against [Kestra](https://kestra.io) doing complex problem solving using workflows
as tools. This system would be running in a Orchestrator that creates new agents in containers.