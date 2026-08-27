# Praxis — Contexto del proyecto (resumen no normativo)

> Este documento es una ayuda de lectura en español. No constituye una segunda
> especificación. En caso de discrepancia prevalecen `AGENTS.md` para las reglas
> de trabajo y `docs/PRAXIS_SPEC.md` para las decisiones de producto y dominio.

## Qué es Praxis

Praxis es un motor determinista de simulación de trading orientado a medir y
entrenar el comportamiento del trader. Reproduce sesiones, aplica reglas
configurables similares a evaluaciones de prop firms y registra decisiones como
eventos de dominio.

No predice precios, no genera señales, no opera dinero real y no presenta
resultados como promesa de rentabilidad. El primer objetivo es construir un
kernel de simulación excelente; el entrenamiento adaptativo solo se considera
después de validarlo con uso personal.

## Decisiones principales

- Go para el kernel; TypeScript para una UI futura; Python solo cuando exista
  trabajo estadístico específico.
- Tipos enteros definidos para precios, dinero, cantidades, P&L y riesgo.
- Tiempo lógico y aleatoriedad inyectada con semilla registrada.
- Ejecución conservadora: ninguna mejora favorable injustificada y el stop
  gana cuando una barra es ambigua.
- Monolito modular con límites hexagonales; el dominio no importa
  infraestructura.
- Un único modelo de eventos ordenados para replay, sintético y live paper.
- MNQ es el instrumento inicial. Cripto puede servir como fuente barata de
  desarrollo, pero no es una decisión de producto abierta para la primera
  versión.
- Event sourcing conductual append-only; la interpretación psicológica ocurre
  posteriormente en analítica, no en ejecución.

## Estado real del repositorio

Al instalar estos documentos, el repositorio solo contenía `README.md`. La
descripción anterior de `domain/` y de 24 tests verdes era un objetivo o
baseline procedente de otro contexto, no un resultado verificado aquí.

Por ello la siguiente porción vertical es la mínima de la Fase 0: definir los
tipos necesarios para ejecutar conservadoramente una orden de mercado sobre una
cotización, escribir primero sus invariantes y añadir casos incrementalmente.

## Reglas de dominio relevantes

El coste de una posición se conserva exactamente en céntimos. Para instrumentos
representables con esa unidad:

```text
coste del fill = precio en ticks * céntimos por tick * cantidad absoluta
```

El valor monetario del tick pertenece a la especificación inmutable del
instrumento. Un precio medio es solo una vista de presentación; no participa en
P&L ni riesgo. Los cierres parciales usan coste medio ponderado, las comisiones
redondean hacia arriba y cualquier redondeo inevitable del P&L perjudica al
trader de forma determinista.

Los eventos que entran al kernel se ordenan por tiempo lógico y, en caso de
empate, por un número de secuencia explícito. Los adaptadores rechazan o
normalizan determinísticamente datos desordenados.

## Roadmap resumido

1. Kernel de ejecución conservadora y sus property tests.
2. Posición y cuenta con coste exacto, cierres, flips, P&L y comisiones.
3. Challenge como máquina de estados, con drawdown y fronteras de sesión.
4. Log conductual append-only y CLI mínima de replay.
5. Experimento personal de 50–100 sesiones con hipótesis pre-registradas.
6. Solo con evidencia favorable: generador calibrado, entrenamiento adaptativo
   y live paper.

La ausencia de patrones conductuales estables reduce Praxis a simulador
personal; no se fuerza una narrativa comercial. Las afirmaciones sobre
licencias de datos o regulación deben verificarse en el momento de necesitarlas.
