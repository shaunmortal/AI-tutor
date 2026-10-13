"""
RAGAS scores for the source-grounded tutor.

Reads eval/results/grounding.jsonl, written by `npm run eval:grounding` in backend/, which ran
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
    python3.10 -m venv eval/.venv && eval/.venv/bin/pip install -r eval/requirements.txt
    ANTHROPIC_API_KEY=... eval/.venv/bin/python eval/ragas_eval.py
"""

import json
import os
import statistics
import sys
from pathlib import Path

from langchain_anthropic import ChatAnthropic
from langchain_community.embeddings import FastEmbedEmbeddings
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


def main() -> int:
    rows = [json.loads(line) for line in (RESULTS / "grounding.jsonl").read_text().splitlines() if line]
    scored = [row for row in rows if row["type"] != "off_material" and row["grounding"] != "none"]

    if not scored:
        print("Nothing to score: run `npm run eval:grounding` first.", file=sys.stderr)
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

    llm = LangchainLLMWrapper(ChatAnthropic(model=JUDGE_MODEL, temperature=0, max_tokens=4096))
    embeddings = LangchainEmbeddingsWrapper(FastEmbedEmbeddings(model_name="BAAI/bge-small-en-v1.5"))

    result = evaluate(
        dataset,
        metrics=[
            Faithfulness(),
            ResponseRelevancy(),
            LLMContextPrecisionWithReference(),
            LLMContextRecall(),
        ],
        llm=llm,
        embeddings=embeddings,
    )

    per_question = result.to_pandas()
    per_question.insert(0, "id", [row["id"] for row in scored])
    per_question.to_csv(RESULTS / "ragas.csv", index=False)

    metric_columns = [c for c in per_question.columns if c not in ("id", "user_input", "retrieved_contexts", "response", "reference")]
    summary = {
        "judge": JUDGE_MODEL,
        "scored_questions": len(scored),
        "excluded_refused_in_material": sum(
            1 for row in rows if row["type"] != "off_material" and row["grounding"] == "none"
        ),
        **{
            column: round(statistics.fmean(v for v in per_question[column] if v == v), 3)
            for column in metric_columns
        },
    }

    (RESULTS / "ragas-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
