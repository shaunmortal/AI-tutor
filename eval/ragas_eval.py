"""
RAGAS scores for the source-grounded tutor.

Reads eval/results/grounding.jsonl, written by `go run ./cmd/evalgrounding` in server/, which ran
the tutor's real retrieval and answering on the team-built test set (eval/testset.json), and
scores the in-material questions with RAGAS:

    faithfulness         are the answer's claims supported by the retrieved excerpts
    answer_relevancy     does the answer address the question
    context_precision    are the retrieved excerpts that matter ranked first (needs a reference)
    context_recall       do the retrieved excerpts contain what the reference answer needs

Off-material questions are not scored here (there is no reference answer to recall); refusal
behaviour on them is measured by the grounding script itself.

Judge: Claude via LangChain. Embeddings for answer relevancy: a small local model (fastembed),
so no second API key is needed.

Usage:
    uv venv -p 3.12 eval/.venv && VIRTUAL_ENV=eval/.venv uv pip install -r eval/requirements.txt
    ANTHROPIC_API_KEY=... eval/.venv/bin/python eval/ragas_eval.py

RAGAS_METRICS=answer_relevancy (comma-separated) re-scores only those metrics and merges them into
the existing results, so one failed metric doesn't mean paying for all four again.
"""

import json
import os

# No usage telemetry from an evaluation run.
os.environ.setdefault("RAGAS_DO_NOT_TRACK", "true")
import statistics
import sys
from pathlib import Path

import pandas as pd
from fastembed import TextEmbedding
from langchain_anthropic import ChatAnthropic
from langchain_core.embeddings import Embeddings
from ragas import EvaluationDataset, evaluate
from ragas.embeddings import LangchainEmbeddingsWrapper
from ragas.llms import LangchainLLMWrapper
from ragas.metrics import (
    Faithfulness,
    LLMContextPrecisionWithReference,
    LLMContextRecall,
    ResponseRelevancy,
)

ROOT = Path(__file__).resolve().parent
RESULTS = ROOT / "results"
JUDGE_MODEL = os.environ.get("RAGAS_JUDGE_MODEL", "claude-sonnet-5-5")

GENERAL_KNOWLEDGE = "General knowledge, not from your course material:"


def source_backed(answer: str) -> str:
    """The part of the answer that claims to come from the course. The labelled general-knowledge
    section is excluded: it is explicitly not grounded, so scoring it for faithfulness to the
    excerpts would penalise the tutor for being honest about where it came from."""
    return answer.split(GENERAL_KNOWLEDGE)[0].strip()


class LocalEmbeddings(Embeddings):
    """fastembed straight into LangChain. langchain-community's FastEmbedEmbeddings no longer
    validates against current fastembed, so the ten lines are written out here."""

    def __init__(self, model: str = "BAAI/bge-small-en-v1.5"):
        self.model = model  # RAGAS reads .model as the model's name
        self._encoder = TextEmbedding(model_name=model)

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [vector.tolist() for vector in self._encoder.embed(texts)]

    def embed_query(self, text: str) -> list[float]:
        return self.embed_documents([text])[0]


METRICS = {
    "faithfulness": Faithfulness,
    "answer_relevancy": ResponseRelevancy,
    "llm_context_precision_with_reference": LLMContextPrecisionWithReference,
    "context_recall": LLMContextRecall,
}


def main() -> int:
    rows = [json.loads(line) for line in (RESULTS / "grounding.jsonl").read_text().splitlines() if line]
    scored = [row for row in rows if row["type"] != "off_material" and row["grounding"] != "none"]

    if not scored:
        print("Nothing to score: run `cd server && go run ./cmd/evalgrounding -user <uuid>` first.", file=sys.stderr)
        return 1

    dataset = EvaluationDataset.from_list(
        [
            {
                "user_input": row["question"],
                "retrieved_contexts": row["contexts"],
                "response": source_backed(row["answer"]),
                "reference": row["reference"],
            }
            for row in scored
        ]
    )

    # Current Claude models reject a non-default temperature, so none is set and RAGAS is told
    # not to inject one.
    llm = LangchainLLMWrapper(ChatAnthropic(model=JUDGE_MODEL, max_tokens=4096), bypass_temperature=True)
    embeddings = LangchainEmbeddingsWrapper(LocalEmbeddings())

    wanted = [m.strip() for m in os.environ.get("RAGAS_METRICS", "").split(",") if m.strip()] or list(METRICS)
    result = evaluate(dataset, metrics=[METRICS[m]() for m in wanted], llm=llm, embeddings=embeddings)

    per_question = result.to_pandas()
    per_question.insert(0, "id", [row["id"] for row in scored])
    previous = RESULTS / "ragas.csv"
    if len(wanted) < len(METRICS) and previous.exists():
        # Merge the re-scored columns into the earlier run, row by row on question id.
        merged = pd.read_csv(previous).set_index("id")
        for column in wanted:
            merged[column] = per_question.set_index("id")[column]
        per_question = merged.reset_index()
    per_question.to_csv(previous, index=False)

    metric_columns = [c for c in per_question.columns if c not in ("id", "user_input", "retrieved_contexts", "response", "reference")]
    summary = {
        "judge": JUDGE_MODEL,
        "scored_questions": len(scored),
        "excluded_refused_in_material": sum(
            1 for row in rows if row["type"] != "off_material" and row["grounding"] == "none"
        ),
        **{
            column: round(statistics.fmean(scores), 3) if (scores := [v for v in per_question[column] if v == v]) else None
            for column in metric_columns
        },
    }

    (RESULTS / "ragas-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
